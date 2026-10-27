package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/storage"
	"github.com/aisidev/tt/model"
)

func TestArchiveDownloadDoesNotHoldLifecycleLockAndCancellationRetainsClaim(t *testing.T) {
	s, _, _ := serverFixture(t)
	s.runtime.store = storage.FileStore{}
	if err := os.MkdirAll(s.runtime.runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
			return
		case <-release:
		}
	}))
	defer source.Close()
	defer once.Do(func() { close(release) })
	t.Setenv("TT_ARCHIVE_SOURCES", strings.TrimPrefix(source.URL, "http://"))
	server := httptest.NewServer(s.Handler(nil))
	defer server.Close()
	identity := "download-g1"
	body, _ := json.Marshal(api.ArchiveImportRequest{Pull: model.ArchivePull{ID: identity, SHA256: strings.Repeat("0", 64), Bytes: 100, ExpandedMiB: 16}, Spec: model.VolumeSpec{ID: identity, Generation: 1, SizeMiB: 16}})
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/volumes/"+identity+"/import", bytes.NewReader(body))
	request.Header.Set("X-Compute-Command", `{"subject":"download-g1","generation":1,"token":1,"key":"download","fingerprint":"`+strings.Repeat("a", 64)+`"}`)
	request.Header.Set("X-Archive-Source", source.URL+"/archive")
	request.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, _ := server.Client().Do(request)
		if res != nil {
			res.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("download did not begin")
	}
	// A blocked transfer cannot consume the global lifecycle mutation window.
	locked := make(chan struct{})
	go func() { s.mutation.Lock(); close(locked); s.mutation.Unlock() }()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("cold lane blocked lifecycle")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("download did not cancel")
	}
	once.Do(func() { close(release) })
	// Handler completion is asynchronous to the disconnected client.
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mutation.Lock()
		active := s.runtime.transferActive[identity]
		s.mutation.Unlock()
		if !active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("transfer ownership was not released")
		}
		time.Sleep(time.Millisecond)
	}
	s.mutation.Lock()
	defer s.mutation.Unlock()
	v, err := s.runtime.volume(context.Background(), identity)
	if err != nil || v.State != "creating" {
		t.Fatal(v, err)
	}
	if _, err = s.runtime.CreateVolume(context.Background(), model.VolumeSpec{ID: identity, Generation: 1, SizeMiB: 16}); err == nil {
		t.Fatal("ordinary create adopted unfinished import")
	}
	if err = s.runtime.DeleteVolume(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	usage, err := archiveUsage(context.Background(), s.runtime.db.conn)
	if err != nil || usage != 0 {
		t.Fatal("explicit cleanup retained scratch claim", usage, err)
	}
}

func TestArchiveCatalogRequiresDurablePublication(t *testing.T) {
	s, _, _ := serverFixture(t)
	ctx := context.Background()
	id := "sha256-" + strings.Repeat("b", 64) + "-g1"
	intent := archiveIntent{ID: id, Digest: strings.Repeat("b", 64), Image: true, State: "pending", Phase: "publishing"}
	if err := saveDisk(ctx, s.runtime.db.conn, "archive_imports", id, intent); err != nil {
		t.Fatal(err)
	}
	images, err := s.publishedArchiveImages(ctx, []string{"fixture", id, "sha256-unowned-g1"})
	if err != nil || len(images) != 1 || images[0] != "fixture" {
		t.Fatal(images, err)
	}
	intent.State = "ready"
	if err = saveDisk(ctx, s.runtime.db.conn, "archive_imports", id, intent); err != nil {
		t.Fatal(err)
	}
	images, err = s.publishedArchiveImages(ctx, []string{"fixture", id})
	if err != nil || len(images) != 2 {
		t.Fatal(images, err)
	}
}

func TestPortableDataImportPublishesOnlyVerifiedBytes(t *testing.T) {
	s, _, _ := serverFixture(t)
	s.runtime.store = storage.FileStore{}
	if err := os.MkdirAll(s.runtime.runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	sourceFile, err := os.CreateTemp(t.TempDir(), "source")
	if err != nil {
		t.Fatal(err)
	}
	defer sourceFile.Close()
	if err = sourceFile.Truncate(1 << 20); err != nil {
		t.Fatal(err)
	}
	if _, err = sourceFile.WriteAt([]byte("archive-content"), 4096); err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if err = storage.WriteDiskArchive(context.Background(), &data, model.DiskArchive{Version: 1, Format: "raw", SizeMiB: 1}, []storage.ArchiveInput{{Name: "disk.raw", File: sourceFile, Bytes: 1 << 20}}); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data.Bytes())
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data.Bytes()) }))
	defer source.Close()
	t.Setenv("TT_ARCHIVE_SOURCES", strings.TrimPrefix(source.URL, "http://"))
	intent, err := s.runtime.prepareArchiveImport(context.Background(), model.ArchivePull{ID: "verified-g1", SHA256: hex.EncodeToString(hash[:]), Bytes: int64(data.Len()), ExpandedMiB: 1}, model.VolumeSpec{ID: "verified-g1", Generation: 1, SizeMiB: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.receiveArchive(context.Background(), source.URL, intent); err != nil {
		t.Fatal(err)
	}
	v, err := s.runtime.volume(context.Background(), intent.ID)
	if err != nil || v.State != "ready" {
		t.Fatal(v, err)
	}
	disk, err := os.Open(filepath.Join(s.runtime.runtimeDir, "volume-"+intent.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	_, _ = disk.Seek(4096, io.SeekStart)
	got := make([]byte, len("archive-content"))
	if _, err = io.ReadFull(disk, got); err != nil || string(got) != "archive-content" {
		t.Fatal(string(got), err)
	}
}

func TestImageRecipeProfileMismatchCannotPublishReady(t *testing.T) {
	s, _, _ := serverFixture(t)
	s.runtime.store = storage.FileStore{}
	sourceFile, err := os.CreateTemp(t.TempDir(), "source")
	if err != nil {
		t.Fatal(err)
	}
	defer sourceFile.Close()
	if err = sourceFile.Truncate(1 << 20); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err = storage.WriteDiskArchive(context.Background(), &archive, model.DiskArchive{Version: 1, Format: "raw", SizeMiB: 1}, []storage.ArchiveInput{{Name: "disk.raw", File: sourceFile, Bytes: 1 << 20}}); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(archive.Bytes())
	hexDigest := hex.EncodeToString(digest[:])
	id := "sha256-" + hexDigest + "-g1"
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive.Bytes()) }))
	defer source.Close()
	t.Setenv("TT_ARCHIVE_SOURCES", strings.TrimPrefix(source.URL, "http://"))
	intent, err := s.runtime.prepareArchiveImport(context.Background(), model.ArchivePull{ID: id, SHA256: hexDigest, Bytes: int64(archive.Len()), ExpandedMiB: 96}, model.VolumeSpec{Engine: model.Firecracker, SizeMiB: 16}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.receiveArchive(context.Background(), source.URL, intent); err == nil {
		t.Fatal("data archive accepted as bootable recipe image")
	}
	current, err := diskRecord[archiveIntent](context.Background(), s.runtime.db.conn, "archive_imports", id)
	if err != nil || current.State != "pending" {
		t.Fatal(current, err)
	}
	if present, err := s.runtime.store.ImageExists(context.Background(), s.runtime.imageDir+"/"+id); err != nil || present {
		t.Fatal("mismatched profile published", present, err)
	}
}

func TestUnpublishedImageCannotInitializeIndependentRoot(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	ctx := context.Background()
	id := "sha256-" + strings.Repeat("d", 64) + "-g1"
	intent := archiveIntent{ID: id, Digest: strings.Repeat("d", 64), Image: true, State: "pending", Phase: "publishing"}
	if err := saveDisk(ctx, r.db.conn, "archive_imports", id, intent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateVolume(ctx, model.VolumeSpec{ID: "root-g1", Generation: 1, SizeMiB: 128, Image: id, Engine: model.Firecracker}); err == nil {
		t.Fatal("independent volume cloned unpublished image")
	}
	volume, err := diskRecord[model.Volume](ctx, r.db.conn, "volumes", "root-g1")
	if err != nil || volume != nil {
		t.Fatal("image refusal allocated a disk", volume, err)
	}
}

func TestDeleteUnadmittedVolumeRequiresPhysicalAbsence(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	ctx := context.Background()
	if err := os.MkdirAll(r.runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	id := "unadmitted-g1"
	if err := r.DeleteVolume(ctx, id); err != nil {
		t.Fatal("confirmed absence was not acknowledged", err)
	}
	path := r.volumePath(id)
	if err := os.WriteFile(path, []byte("unowned-user-data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteVolume(ctx, id); err == nil {
		t.Fatal("missing row permitted unowned byte deletion")
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "unowned-user-data" {
		t.Fatal("unowned data was changed", err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(t.TempDir(), "missing"), path); err != nil {
		t.Fatal(err)
	}
	if err = r.DeleteVolume(ctx, id); err == nil {
		t.Fatal("dangling unowned link was mistaken for absence")
	}
}

func TestAbortUnpublishedRetainedImportCannotDestroyReadyData(t *testing.T) {
	s, _, _ := serverFixture(t)
	s.runtime.store = storage.FileStore{}
	if err := os.MkdirAll(s.runtime.runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pending := "pending-import-g1"
	intent, err := s.runtime.prepareArchiveImport(ctx, model.ArchivePull{ID: pending, SHA256: strings.Repeat("f", 64), Bytes: 100, ExpandedMiB: 1}, model.VolumeSpec{ID: pending, Generation: 1, SizeMiB: 1, Retain: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	intent.Phase = "publishing"
	if err = saveDisk(ctx, s.runtime.db.conn, "archive_imports", pending, intent); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.runtime.volumePath(pending), []byte("owned unpublished bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	ready := "ready-import-g1"
	completed := intent
	completed.ID = ready
	completed.State = "ready"
	completed.Phase = "ready"
	completed.Spec.ID = ready
	if err = saveDisk(ctx, s.runtime.db.conn, "archive_imports", ready, completed); err != nil {
		t.Fatal(err)
	}
	volume := model.Volume{ID: ready, Owner: "owner", HostID: s.runtime.hostID, Generation: 1, SizeMiB: 1, Format: "raw", Retain: true, State: "ready", Phase: "ready", CreatedAt: 1}
	if err = saveDisk(ctx, s.runtime.db.conn, "volumes", ready, volume); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.runtime.volumePath(ready), []byte("ready user bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s.Handler(nil))
	defer server.Close()
	for _, id := range []string{pending, ready} {
		request, _ := http.NewRequest("POST", server.URL+"/api/volumes/"+id+"/import/abort", nil)
		request.Header.Set("X-Compute-Command", `{"subject":"`+id+`","generation":1,"token":2,"key":"abort","fingerprint":"`+strings.Repeat("a", 64)+`"}`)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if id == pending && response.StatusCode != 200 {
			t.Fatal("unpublished retention prevented cleanup", response.StatusCode)
		}
		if id == ready && response.StatusCode != 409 {
			t.Fatal("ready retained import was discarded", response.StatusCode)
		}
	}
	if _, err = os.Lstat(s.runtime.volumePath(pending)); !os.IsNotExist(err) {
		t.Fatal("unpublished target still exists", err)
	}
	data, err := os.ReadFile(s.runtime.volumePath(ready))
	if err != nil || string(data) != "ready user bytes" {
		t.Fatal("ready disk was changed", err)
	}
}

func TestImportedContentRootBootPlanDoesNotRequireTheSourceCache(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	ctx := context.Background()
	image := "sha256-" + strings.Repeat("e", 64) + "-g1"
	root := model.Volume{ID: "portable-root-g1", Owner: "env", HostID: r.hostID, Generation: 1, SizeMiB: 128, Image: image, Engine: model.Qemu, Format: "qcow2", Retain: true, State: "ready", Phase: "ready", CreatedAt: 1}
	if err := saveDisk(ctx, r.db.conn, "volumes", root.ID, root); err != nil {
		t.Fatal(err)
	}
	request := requestFixture()
	request.Image = image
	request.RootVolumeID = root.ID
	request.InitialStopped = true
	request.Ports = nil
	vm, err := r.CreateVM(managedContext("env", 1), request)
	if err != nil || vm.State != model.VmStopped || vm.Options.RootVolumeID != root.ID {
		t.Fatal("verified root depended on an absent source image cache", vm, err)
	}
}
