package agent

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/storage"
	"github.com/aisidev/tt/model"
)

func (r *Runtime) archiveGuard(id string) error {
	if r.exportReaders[id] > 0 || r.transferActive[id] {
		return diskError("state", "transfer_busy", "disk has an active export")
	}
	return nil
}
func (r *Runtime) openArchive(ctx context.Context, id string) (model.DiskArchive, []storage.ArchiveInput, error) {
	v, err := r.volume(ctx, id)
	if err != nil {
		return model.DiskArchive{}, nil, err
	}
	if v.State != "ready" || v.AttachedTo != "" || v.PendingGroup != "" || v.PendingSnapshot != "" {
		return model.DiskArchive{}, nil, diskError("state", "volume_busy", "archive export requires an unattached settled disk")
	}
	manifest := model.DiskArchive{Version: 1, Engine: v.Engine, Image: v.Image, Format: v.Format, SizeMiB: v.SizeMiB}
	inputs := []storage.ArchiveInput{}
	add := func(name, path string, size int64) error {
		file, e := os.Open(path)
		if e != nil {
			return e
		}
		info, e := file.Stat()
		if e != nil {
			file.Close()
			return e
		}
		if size < 0 {
			if !info.Mode().IsRegular() {
				file.Close()
				return fmt.Errorf("archive member is not regular")
			}
			size = info.Size()
		}
		inputs = append(inputs, storage.ArchiveInput{Name: name, File: file, Bytes: size})
		return nil
	}
	closeAll := func() {
		for _, input := range inputs {
			input.File.Close()
		}
	}
	path := r.volumePath(id)
	switch v.Engine {
	case model.Firecracker:
		dir, e := r.store.FirecrackerDir(ctx, path)
		if e != nil {
			return manifest, nil, e
		}
		if err = add("vmlinux", filepath.Join(dir, "vmlinux"), -1); err != nil {
			break
		}
		root := filepath.Join(dir, "rootfs.ext4")
		if r.storage == model.Zvol {
			root, err = storage.ResolveVolumeDevice(ctx, r.store, path+"/rootfs")
		}
		if err != nil {
			break
		}
		err = add("rootfs.ext4", root, int64(v.SizeMiB)<<20)
		if err != nil {
			break
		}
		config := filepath.Join(dir, "guest-config.ext4")
		if _, e := os.Stat(config); e == nil {
			err = add("guest-config.ext4", config, -1)
		} else if !os.IsNotExist(e) {
			err = e
		}
	case model.Qemu:
		disk, e := r.store.ResolveDisk(ctx, path)
		if e != nil {
			err = e
			break
		}
		name := "disk.raw"
		size := int64(v.SizeMiB) << 20
		if v.Format == "qcow2" {
			name = "disk.qcow2"
			size = -1
		}
		err = add(name, disk, size)
	default:
		disk := path
		if r.storage == model.Zvol {
			disk, err = storage.ResolveVolumeDevice(ctx, r.store, path)
		}
		if err == nil {
			err = add("disk.raw", disk, int64(v.SizeMiB)<<20)
		}
	}
	if err != nil {
		closeAll()
		return manifest, nil, err
	}
	manifest.Members = []model.ArchiveMember{}
	for _, input := range inputs {
		manifest.Members = append(manifest.Members, model.ArchiveMember{Name: input.Name, Bytes: input.Bytes})
	}
	if err = storage.ValidDiskArchive(manifest); err != nil {
		closeAll()
		return manifest, nil, err
	}
	if r.exportReaders == nil {
		r.exportReaders = map[string]int{}
	}
	r.exportReaders[id]++
	return manifest, inputs, nil
}
func (s *Server) exportArchiveHTTP(w http.ResponseWriter, request *http.Request) {
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		failure(w, diskError("rate_limited", "transfer_busy", "archive lane is full"))
		return
	}
	type opened struct {
		manifest model.DiskArchive
		inputs   []storage.ArchiveInput
	}
	value, err := mutate(s, func(ctx context.Context, r *Runtime) (opened, error) {
		manifest, inputs, e := r.openArchive(ctx, request.PathValue("id"))
		return opened{manifest, inputs}, e
	})
	if err != nil {
		failure(w, err)
		return
	}
	defer func() {
		for _, input := range value.inputs {
			input.File.Close()
		}
		s.mutation.Lock()
		s.runtime.exportReaders[request.PathValue("id")]--
		s.mutation.Unlock()
	}()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Minute)
	defer cancel()
	if err = storage.WriteDiskArchive(ctx, w, value.manifest, value.inputs); err != nil {
		panic(http.ErrAbortHandler)
	}
}

type archiveIntent struct {
	ID          string           `json:"id"`
	Digest      string           `json:"digest"`
	Bytes       int64            `json:"bytes"`
	ExpandedMiB uint32           `json:"expanded_mib"`
	Spec        model.VolumeSpec `json:"spec"`
	Image       bool             `json:"image"`
	State       string           `json:"state"`
	Phase       string           `json:"phase"`
}

func validPull(pull model.ArchivePull) bool {
	_, err := hex.DecodeString(pull.SHA256)
	return err == nil && len(pull.SHA256) == 64 && pull.Bytes > 0 && pull.Bytes <= 1<<40 && pull.ExpandedMiB > 0 && pull.ExpandedMiB <= (1<<20)+storage.ArchiveOverheadMiB
}
func archiveSource(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", diskError("terminal", "invalid_source", "invalid archive source")
	}
	allowed := false
	for _, host := range strings.Split(os.Getenv("TT_ARCHIVE_SOURCES"), ",") {
		if strings.TrimSpace(host) == u.Host {
			allowed = true
		}
	}
	if !allowed {
		return "", diskError("denied", "archive_source_denied", "archive origin is not configured for this executor")
	}
	return u.String(), nil
}
func (r *Runtime) prepareArchiveImport(ctx context.Context, pull model.ArchivePull, spec model.VolumeSpec, image bool) (archiveIntent, error) {
	if !validPull(pull) || model.ValidateName(pull.ID, "archive") != nil {
		return archiveIntent{}, diskError("terminal", "invalid_archive", "invalid archive budget or identity")
	}
	var raw string
	err := r.db.conn.QueryRowContext(ctx, "SELECT data FROM archive_imports WHERE id=?1", pull.ID).Scan(&raw)
	if image && (spec.Engine != "" || spec.SizeMiB != 0) && ((spec.Engine != model.Qemu && spec.Engine != model.Firecracker) || spec.SizeMiB == 0 || spec.SizeMiB > 1<<20) {
		return archiveIntent{}, diskError("terminal", "invalid_archive", "invalid expected image profile")
	}
	intent := archiveIntent{ID: pull.ID, Digest: pull.SHA256, Bytes: pull.Bytes, ExpandedMiB: pull.ExpandedMiB, Spec: spec, Image: image, State: "pending", Phase: "downloading"}
	if err == nil {
		var existing archiveIntent
		if json.Unmarshal([]byte(raw), &existing) != nil {
			return intent, diskError("terminal", "malformed", "malformed archive journal")
		}
		if existing.Digest != intent.Digest || existing.Bytes != intent.Bytes || existing.ExpandedMiB != intent.ExpandedMiB || existing.Image != intent.Image || !sameArchiveSpec(existing.Spec, spec) {
			return intent, diskError("terminal", "fingerprint_conflict", "archive identity has a different intent")
		}
		if existing.State == "deleting" {
			return intent, diskError("state", "archive_busy", "archive cache destruction is incomplete")
		}
		if existing.State == "deleted" {
			if !image {
				return intent, diskError("state", "archive_deleted", "archive import was explicitly deleted")
			}
			if err = r.recount(ctx); err != nil {
				return intent, err
			}
			if !r.resource.CanFit(0, 0, model.SatAdd(pull.ExpandedMiB, pull.ExpandedMiB)) {
				return intent, diskError("transient", "capacity_exceeded", "archive staging budget is unavailable")
			}
			if err = saveDisk(ctx, r.db.conn, "archive_imports", pull.ID, intent); err != nil {
				return intent, err
			}
			return intent, nil
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return intent, err
	}
	if image {
		if pull.ID != "sha256-"+pull.SHA256+"-g1" {
			return intent, diskError("terminal", "invalid_archive", "image identity must be content addressed")
		}
		present, e := r.store.ImageExists(ctx, r.imageDir+"/"+pull.ID)
		if e != nil {
			return intent, e
		}
		if present {
			return intent, diskError("state", "image_unowned", "image path has no matching import journal")
		}
	} else {
		if spec.ID != pull.ID || spec.Generation == 0 || spec.SizeMiB == 0 || spec.SizeMiB > 1<<20 || !strings.HasSuffix(spec.ID, fmt.Sprintf("-g%d", spec.Generation)) || (spec.HostID != "" && spec.HostID != r.hostID) || (spec.Engine != "" && spec.Engine != model.Qemu && spec.Engine != model.Firecracker) || ((spec.Engine == "") != (spec.Image == "")) || spec.FromSnapshot != "" {
			return intent, diskError("terminal", "invalid_volume", "archive volume specification is invalid")
		}
		existing, e := diskRecord[model.Volume](ctx, r.db.conn, "volumes", spec.ID)
		if e != nil {
			return intent, e
		}
		if existing != nil {
			return intent, diskError("state", "volume_busy", "archive import requires a new volume")
		}
	}
	if err = r.recount(ctx); err != nil {
		return intent, err
	}
	claim := pull.ExpandedMiB
	if image {
		claim = model.SatAdd(claim, pull.ExpandedMiB)
	}
	if !image {
		claim = model.SatAdd(claim, (model.Volume{SizeMiB: spec.SizeMiB, Image: spec.Image, State: "creating"}).ReservedMiB())
	}
	if !r.resource.CanFit(0, 0, claim) {
		return intent, diskError("transient", "capacity_exceeded", "archive staging budget is unavailable")
	}
	tx, e := r.db.conn.BeginTx(ctx, nil)
	if e != nil {
		return intent, e
	}
	defer tx.Rollback()
	if !image {
		format := "raw"
		if spec.Engine == model.Qemu && r.storage == model.File {
			format = "qcow2"
		}
		v := model.Volume{ID: spec.ID, Owner: spec.Owner, HostID: r.hostID, Generation: spec.Generation, SizeMiB: spec.SizeMiB, Engine: spec.Engine, Image: spec.Image, Format: format, Retain: spec.Retain, State: "creating", Phase: "archive", CreatedAt: r.now()}
		if e = saveDisk(ctx, tx, "volumes", v.ID, v); e != nil {
			return intent, e
		}
	}
	data, _ := json.Marshal(intent)
	if _, e = tx.ExecContext(ctx, "INSERT INTO archive_imports(id,data) VALUES(?1,?2)", pull.ID, string(data)); e != nil {
		return intent, e
	}
	return intent, tx.Commit()
}
func sameArchiveSpec(a, b model.VolumeSpec) bool { return reflect.DeepEqual(a, b) }
func (s *Server) importArchiveHTTP(w http.ResponseWriter, request *http.Request) {
	if !requireDiskIdentity(w, request) {
		return
	}
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		failure(w, diskError("rate_limited", "transfer_busy", "archive lane is full"))
		return
	}
	var body api.ArchiveImportRequest
	body, ok := api.ReadJSON[api.ArchiveImportRequest](w, request)
	if !ok {
		return
	}
	image := strings.HasPrefix(request.URL.Path, "/api/images/")
	if image {
		body.Pull.ID = request.PathValue("id")
	} else {
		body.Spec.ID = request.PathValue("id")
		body.Pull.ID = body.Spec.ID
	}
	source, err := archiveSource(request.Header.Get("X-Archive-Source"))
	if err != nil {
		failure(w, err)
		return
	}
	intent, err := mutate(s, func(ctx context.Context, r *Runtime) (archiveIntent, error) {
		return r.prepareArchiveImport(ctx, body.Pull, body.Spec, image)
	})
	if err != nil {
		failure(w, err)
		return
	}
	s.mutation.Lock()
	if s.runtime.transferActive == nil {
		s.runtime.transferActive = map[string]bool{}
	}
	s.runtime.transferActive[intent.ID] = true
	s.mutation.Unlock()
	defer func() { s.mutation.Lock(); delete(s.runtime.transferActive, intent.ID); s.mutation.Unlock() }()
	if intent.State == "ready" {
		s.archiveImportReply(w, request, image)
		return
	}
	if err = s.receiveArchive(request.Context(), source, intent); err != nil {
		failure(w, diskError("unknown", "archive_incomplete", "archive import retained its intent and reservation for retry or explicit cleanup"))
		return
	}
	if image {
		s.RefreshImages(request.Context())
	}
	s.archiveImportReply(w, request, image)
}
func (s *Server) archiveImportReply(w http.ResponseWriter, request *http.Request, image bool) {
	if image {
		api.WriteJSON(w, 200, api.Success(map[string]any{"id": request.PathValue("id"), "state": "ready"}))
		return
	}
	v, err := ReadDiskRecord[model.Volume](request.Context(), s.dbPath, "volumes", request.PathValue("id"))
	if err != nil || v == nil {
		if err == nil {
			err = diskError("unknown", "volume_incomplete", "archive volume is unavailable")
		}
		failure(w, err)
		return
	}
	api.WriteJSON(w, 201, api.Success(*v))
}
func (s *Server) receiveArchive(parent context.Context, source string, intent archiveIntent) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()
	stage := filepath.Join(filepath.Dir(s.dbPath), "archive-stages", intent.ID)
	if err := os.MkdirAll(filepath.Dir(stage), 0700); err != nil {
		return err
	}
	// A non-ready journal owns this unpublished stage. It was never available to
	// any VM, so a restart can discard and re-download it without data inference.
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, "GET", source, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	if err != nil {
		return diskError("transient", "archive_unavailable", "archive source unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.ContentLength > intent.Bytes {
		return diskError("terminal", "archive_unavailable", "archive source did not meet the admitted envelope")
	}
	manifest, err := storage.ExtractDiskArchive(ctx, response.Body, stage, intent.Digest, intent.Bytes, intent.ExpandedMiB)
	if err != nil {
		fmt.Fprintf(s.runtime.db.stderr, "[agent] archive validation failed before publication: %v\n", err)
		return diskError("terminal", "invalid_archive", "archive validation failed before publication")
	}
	if intent.Image && intent.Spec.Engine != "" && (manifest.Engine != intent.Spec.Engine || manifest.SizeMiB != intent.Spec.SizeMiB) {
		return diskError("terminal", "image_profile_mismatch", "image archive differs from its admitted profile")
	}
	if intent.Image && manifest.SizeMiB > intent.ExpandedMiB {
		return diskError("terminal", "invalid_archive", "image virtual capacity exceeds the admitted reservation")
	}
	if !intent.Image && (manifest.SizeMiB != intent.Spec.SizeMiB || manifest.Engine != intent.Spec.Engine || manifest.Image != intent.Spec.Image) {
		return diskError("terminal", "invalid_archive", "archive does not match its disk intent")
	}
	type publication struct {
		target  string
		store   storage.ImageStore
		cleanup bool
	}
	setup, err := mutate(s, func(work context.Context, r *Runtime) (publication, error) {
		target := r.volumePath(intent.ID)
		if intent.Image {
			target = r.imageDir + "/" + intent.ID
		}
		cleanup := intent.Phase == "publishing"
		intent.Phase = "publishing"
		data, _ := json.Marshal(intent)
		_, e := r.db.conn.ExecContext(work, "UPDATE archive_imports SET data=?2 WHERE id=?1", intent.ID, string(data))
		return publication{target, r.store, cleanup}, e
	})
	if err != nil {
		return err
	}
	if setup.cleanup {
		if err = setup.store.RemoveImage(ctx, setup.target); err != nil {
			return err
		}
	}
	if err = storage.PublishDiskArchive(ctx, setup.store, stage, setup.target, manifest, intent.Image); err != nil {
		fmt.Fprintf(s.runtime.db.stderr, "[agent] archive publication retained for retry: %v\n", err)
		return diskError("unknown", "archive_incomplete", "archive publication retained for retry")
	}
	if err = os.RemoveAll(stage); err != nil {
		return err
	}
	_, err = mutate(s, func(work context.Context, r *Runtime) (model.Unit, error) {
		tx, e := r.db.conn.BeginTx(work, nil)
		if e != nil {
			return model.Unit{}, e
		}
		defer tx.Rollback()
		if !intent.Image {
			v, e := r.volume(work, intent.ID)
			if e != nil {
				return model.Unit{}, e
			}
			v.State = "ready"
			v.Phase = "ready"
			v.Revision++
			if e = saveDisk(work, tx, "volumes", v.ID, v); e != nil {
				return model.Unit{}, e
			}
		}
		intent.State = "ready"
		intent.Phase = "ready"
		data, _ := json.Marshal(intent)
		if _, e = tx.ExecContext(work, "UPDATE archive_imports SET data=?2 WHERE id=?1", intent.ID, string(data)); e != nil {
			return model.Unit{}, e
		}
		return model.Unit{}, tx.Commit()
	})
	return err
}

const archiveSchema = `CREATE TABLE archive_imports(id TEXT PRIMARY KEY,data TEXT NOT NULL);`

func (r *Runtime) imageImportGuard(ctx context.Context, image string) error {
	if !strings.HasPrefix(image, "sha256-") {
		return nil
	}
	var raw string
	if err := r.db.conn.QueryRowContext(ctx, "SELECT data FROM archive_imports WHERE id=?1", image).Scan(&raw); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return diskError("transient", "unavailable", "image import journal unavailable")
		}
		return diskError("state", "image_not_ready", "content image is not admitted")
	}
	var intent archiveIntent
	if json.Unmarshal([]byte(raw), &intent) != nil {
		return diskError("terminal", "malformed", "malformed image journal")
	}
	if !intent.Image || intent.State != "ready" {
		return diskError("state", "image_not_ready", "content image publication is incomplete")
	}
	return nil
}

func archiveUsage(ctx context.Context, q sqlQuery) (uint32, error) {
	rows, err := q.QueryContext(ctx, "SELECT data FROM archive_imports")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var total uint32
	for rows.Next() {
		var raw sqlText
		if err = rows.Scan(&raw); err != nil {
			return 0, err
		}
		var intent archiveIntent
		if json.Unmarshal([]byte(raw), &intent) != nil || intent.ID == "" {
			return 0, diskError("terminal", "malformed", "malformed archive reservation")
		}
		if intent.State == "pending" || (intent.State == "deleting" && intent.Phase != "ready") {
			total = model.SatAdd(total, intent.ExpandedMiB)
			if intent.Image {
				total = model.SatAdd(total, intent.ExpandedMiB)
			}
		} else if intent.Image && intent.State != "deleted" {
			total = model.SatAdd(total, intent.ExpandedMiB)
		}
	}
	return total, rows.Err()
}

// Archive catalog publication follows the durable ready marker, even if storage
// directory creation becomes visible before the final journal transaction.
func (s *Server) publishedArchiveImages(ctx context.Context, images []string) ([]string, error) {
	db, err := openDatabase(ctx, s.dbPath, true, 2000)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	filtered := make([]string, 0, len(images))
	for _, image := range images {
		if !strings.HasPrefix(image, "sha256-") {
			filtered = append(filtered, image)
			continue
		}
		var raw string
		err = db.conn.QueryRowContext(ctx, "SELECT data FROM archive_imports WHERE id=?1", image).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var intent archiveIntent
		if json.Unmarshal([]byte(raw), &intent) != nil || intent.ID != image {
			return nil, diskError("terminal", "malformed", "malformed image import journal")
		}
		if intent.Image && intent.State == "ready" {
			filtered = append(filtered, image)
		}
	}
	return filtered, nil
}
func (r *Runtime) deleteArchiveStage(ctx context.Context, id string) error {
	intent, err := diskRecord[archiveIntent](ctx, r.db.conn, "archive_imports", id)
	if err != nil || intent == nil {
		return err
	}
	if intent.Image {
		return diskError("state", "archive_busy", "disk identity belongs to an image")
	}
	// Explicit disk destruction owns both the unpublished stage and its claim.
	path := r.archiveDatabasePath(ctx)
	if path == "" {
		return diskError("unknown", "unavailable", "archive stage ownership unavailable")
	}
	if err = os.RemoveAll(filepath.Join(filepath.Dir(path), "archive-stages", id)); err != nil {
		return err
	}
	intent.State = "deleted"
	intent.Phase = "deleted"
	return saveDisk(ctx, r.db.conn, "archive_imports", id, intent)
}

func (r *Runtime) archiveDatabasePath(ctx context.Context) string {
	var sequence int
	var name, path string
	if r.db.conn.QueryRowContext(ctx, "PRAGMA database_list").Scan(&sequence, &name, &path) != nil {
		return ""
	}
	return path
}

func (s *Server) deleteImageCacheHTTP(w http.ResponseWriter, request *http.Request) {
	if !requireDiskIdentity(w, request) {
		return
	}
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		failure(w, diskError("rate_limited", "transfer_busy", "archive lane is full"))
		return
	}
	id := request.PathValue("id")
	type cacheDrop struct {
		intent *archiveIntent
		store  storage.ImageStore
		path   string
	}
	drop, err := mutate(s, func(ctx context.Context, r *Runtime) (cacheDrop, error) {
		intent, err := diskRecord[archiveIntent](ctx, r.db.conn, "archive_imports", id)
		if err != nil {
			return cacheDrop{}, err
		}
		if intent == nil || intent.State == "deleted" {
			if len(id) != 74 || !strings.HasPrefix(id, "sha256-") || !strings.HasSuffix(id, "-g1") {
				return cacheDrop{}, diskError("terminal", "invalid_archive", "invalid image identity")
			}
			entries, e := storage.InventoryNamespace(ctx, r.store, r.imageDir)
			if e != nil {
				return cacheDrop{}, e
			}
			for _, entry := range entries {
				if entry.Name == id || strings.HasPrefix(entry.Name, id+"/") || strings.HasPrefix(entry.Name, id+"@") {
					return cacheDrop{}, diskError("unknown", "image_unowned", "unowned image bytes require quarantine")
				}
			}
			stage := filepath.Join(filepath.Dir(s.dbPath), "archive-stages", id)
			if _, e = os.Lstat(stage); e == nil || !errors.Is(e, os.ErrNotExist) {
				return cacheDrop{}, diskError("unknown", "image_unowned", "unowned image stage requires quarantine")
			}
			if intent == nil {
				intent = &archiveIntent{ID: id, Digest: id[7:71], Image: true, State: "deleted", Phase: "deleted"}
				if e = saveDisk(ctx, r.db.conn, "archive_imports", id, intent); e != nil {
					return cacheDrop{}, e
				}
			}
			return cacheDrop{intent: intent}, nil
		}
		if !intent.Image {
			return cacheDrop{}, diskError("state", "identity_mismatch", "identity belongs to a disk")
		}
		volumes, err := diskList[model.Volume](ctx, r.db.conn, "volumes")
		if err != nil {
			return cacheDrop{}, err
		}
		for _, v := range volumes {
			if v.State != "deleted" && v.Image == id {
				return cacheDrop{}, diskError("state", "image_dependency", "a disk still depends on this image")
			}
		}
		vms, err := r.ListVMs(ctx)
		if err != nil {
			return cacheDrop{}, err
		}
		for _, vm := range vms {
			if vm.Image == id {
				return cacheDrop{}, diskError("state", "image_dependency", "a VM still depends on this image")
			}
		}
		if intent.State != "deleted" {
			intent.State = "deleting"
			if err = saveDisk(ctx, r.db.conn, "archive_imports", id, intent); err != nil {
				return cacheDrop{}, err
			}
		}
		return cacheDrop{intent, r.store, r.imageDir + "/" + id}, nil
	})
	if err != nil {
		failure(w, err)
		return
	}
	if drop.store == nil {
		api.WriteJSON(w, 200, api.Empty())
		return
	}
	if err = drop.store.RemoveImage(request.Context(), drop.path); err != nil {
		failure(w, diskError("unknown", "archive_incomplete", "image cache destruction retained for retry"))
		return
	}
	stage := filepath.Join(filepath.Dir(s.dbPath), "archive-stages", id)
	if err = os.RemoveAll(stage); err != nil {
		failure(w, diskError("unknown", "archive_incomplete", "image staging cleanup retained for retry"))
		return
	}
	_, err = mutate(s, func(ctx context.Context, r *Runtime) (model.Unit, error) {
		drop.intent.State = "deleted"
		drop.intent.Phase = "deleted"
		if err := saveDisk(ctx, r.db.conn, "archive_imports", id, drop.intent); err != nil {
			return model.Unit{}, err
		}
		return model.Unit{}, r.recount(ctx)
	})
	if err != nil {
		failure(w, err)
		return
	}
	s.RefreshImages(request.Context())
	api.WriteJSON(w, 200, api.Empty())
}

// Abort only a durable unpublished import, including its tentative retention.
// Ready disks require the normal retention-aware destruction path; unowned
// bytes and failed probes never turn into absence acknowledgements.
func (s *Server) abortArchiveHTTP(w http.ResponseWriter, request *http.Request) {
	if !requireDiskIdentity(w, request) {
		return
	}
	_, err := mutate(s, func(ctx context.Context, r *Runtime) (model.Unit, error) {
		id := request.PathValue("id")
		if err := r.archiveGuard(id); err != nil {
			return model.Unit{}, err
		}
		intent, err := diskRecord[archiveIntent](ctx, r.db.conn, "archive_imports", id)
		if err != nil {
			return model.Unit{}, err
		}
		if intent == nil {
			return model.Unit{}, r.confirmUnadmittedVolumeAbsent(ctx, id)
		}
		if intent.Image {
			return model.Unit{}, diskError("state", "identity_mismatch", "import identity belongs to an image")
		}
		if intent.State == "deleted" {
			return model.Unit{}, nil
		}
		if intent.State != "pending" {
			return model.Unit{}, diskError("state", "volume_published", "ready import is governed by its disk retention")
		}
		volume, err := r.volume(ctx, id)
		if err != nil {
			return model.Unit{}, err
		}
		if volume.AttachedTo != "" || (volume.State != "creating" && volume.State != "deleting") || volume.Phase != "archive" && volume.Phase != "deleting" {
			return model.Unit{}, diskError("unknown", "volume_unsettled", "import ownership cannot be established")
		}
		volume.Retain = false
		if err = r.saveVolume(ctx, volume); err != nil {
			return model.Unit{}, err
		}
		return model.Unit{}, r.DeleteVolume(ctx, id)
	})
	actionReply(w, err)
}
