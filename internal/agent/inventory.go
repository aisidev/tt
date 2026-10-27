package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/engine"
	"github.com/aisidev/tt/internal/netcfg"
	"github.com/aisidev/tt/internal/storage"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

type inventoryCache struct {
	snapshot model.PhysicalInventory
	expires  time.Time
}

func inventoryKey(kind, id string) string { return kind + "/" + id }
func (s *Server) physicalInventory(ctx context.Context) (model.PhysicalInventory, error) {
	snapshot := model.PhysicalInventory{ID: uuid.NewString(), HostID: s.info.HostID, Items: []model.InventoryItem{}, MaxPageSize: 100}
	control, err := s.commandJournal.ControlReport()
	if err != nil {
		return snapshot, err
	}
	snapshot.Authority = control.Authority
	snapshot.Epoch = control.Epoch
	snapshot.Frozen = control.Enabled && (control.Phase == "fenced" || control.Phase == "inventory" || control.Phase == "reads")
	db, err := openDatabase(ctx, s.dbPath, true, 2000)
	if err != nil {
		return snapshot, err
	}
	defer db.Close()
	var before uint64
	if err = db.conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&before); err != nil {
		return snapshot, err
	}
	items := map[string]model.InventoryItem{}
	active := 0
	knownVolumes := map[string]model.Volume{}
	knownPoints := map[string]model.VolumeSnapshot{}
	physicalParts := 0
	addMeta := func(kind, id, state string, generation uint64) error {
		active++
		if active > storage.MaxNamespaceEntries {
			return diskError("terminal", "size_exceeded", "catalog inventory exceeds its declared budget")
		}
		key := inventoryKey(kind, id)
		item := items[key]
		item.Kind = kind
		item.ID = id
		item.Metadata = true
		item.State = state
		item.Generation = generation
		item.Parts = []string{}
		items[key] = item
		return nil
	}
	for _, table := range []string{"volumes", "volume_snapshots", "archive_imports"} {
		var malformed bool
		if err = db.conn.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE NOT json_valid(data) OR json_type(data,'$.state') IS NOT 'text' OR json_extract(data,'$.state')='')").Scan(&malformed); err != nil {
			return snapshot, diskError("unknown", "inventory_unavailable", "catalog inventory unavailable")
		}
		if malformed {
			return snapshot, diskError("terminal", "malformed", "catalog contains unreadable ownership")
		}
		rows, err := db.conn.QueryContext(ctx, "SELECT id,data FROM "+table+" WHERE json_extract(data,'$.state')!='deleted' ORDER BY id LIMIT ?1", storage.MaxNamespaceEntries+1)
		if err != nil {
			return snapshot, err
		}
		for rows.Next() {
			var id, raw sqlText
			if err = rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return snapshot, err
			}
			switch table {
			case "volumes":
				var v model.Volume
				if json.Unmarshal([]byte(raw), &v) != nil || v.ID != string(id) || v.Generation == 0 {
					rows.Close()
					return snapshot, diskError("terminal", "malformed", "volume inventory metadata is malformed")
				}
				knownVolumes[v.ID] = v
				err = addMeta("volume", v.ID, v.State, v.Generation)
				item := items[inventoryKey("volume", v.ID)]
				item.ClaimMiB = v.ReservedMiB()
				item.Unsettled = v.PendingGroup != "" || v.PendingSnapshot != "" || v.PendingSizeMiB != 0
				item.Subject, item.Parent = v.AttachedEnv, v.SourceSnapshot
				items[inventoryKey("volume", v.ID)] = item
			case "volume_snapshots":
				var p model.VolumeSnapshot
				if json.Unmarshal([]byte(raw), &p) != nil || p.ID != string(id) {
					rows.Close()
					return snapshot, diskError("terminal", "malformed", "checkpoint inventory metadata is malformed")
				}
				knownPoints[p.ID] = p
				err = addMeta("snapshot", p.ID, p.State, p.Generation)
				item := items[inventoryKey("snapshot", p.ID)]
				item.ClaimMiB = p.Source.ReservedMiB()
				item.Parent = p.VolumeID
				items[inventoryKey("snapshot", p.ID)] = item
			case "archive_imports":
				var a archiveIntent
				if json.Unmarshal([]byte(raw), &a) != nil || a.ID != string(id) {
					rows.Close()
					return snapshot, diskError("terminal", "malformed", "archive inventory metadata is malformed")
				}
				if a.Image {
					err = addMeta("image", a.ID, a.State, 1)
					item := items[inventoryKey("image", a.ID)]
					item.ClaimMiB = a.ExpandedMiB
					items[inventoryKey("image", a.ID)] = item
				}
				if err == nil && a.State == "pending" {
					err = addMeta("stage", a.ID, a.State, a.Spec.Generation)
					item := items[inventoryKey("stage", a.ID)]
					item.ClaimMiB = a.ExpandedMiB
					items[inventoryKey("stage", a.ID)] = item
				}
			}
			if err != nil {
				rows.Close()
				return snapshot, err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return snapshot, err
		}
	}
	vms, corrupt, err := ReadRecoverySnapshot(ctx, s.dbPath, s.runtime.db.stderr)
	if err != nil {
		return snapshot, err
	}
	if corrupt {
		return snapshot, diskError("terminal", "malformed", "VM inventory contains unreadable ownership")
	}
	for _, vm := range vms {
		if err = addMeta("runtime", vm.ID, string(vm.State), 0); err != nil {
			return snapshot, err
		}
		item := items[inventoryKey("runtime", vm.ID)]
		item.Subject = vm.EnvID
		item.Engine, item.Image, item.CPU, item.MemMiB = vm.Engine, vm.Image, vm.CPU, vm.Mem
		item.RootVolumeID = vm.Options.RootVolumeID
		for _, disk := range vm.Options.DataVolumes {
			item.DataVolumeIDs = append(item.DataVolumeIDs, disk.ID)
		}
		sort.Strings(item.DataVolumeIDs)
		if at := strings.LastIndex(vm.EnvID, "-g"); at >= 0 {
			item.Generation, _ = strconv.ParseUint(vm.EnvID[at+2:], 10, 64)
		}
		item.Unsettled = vm.PendingResources != nil || vm.Backup.HasArtifacts() || vm.Options.RootManaged && !vm.Options.CreationComplete
		items[inventoryKey("runtime", vm.ID)] = item
		if !vm.Options.RootManaged && vm.Engine != model.Docker && vm.Engine != model.Jail {
			if err = addMeta("vm_disk", vm.ID, string(vm.State), 0); err != nil {
				return snapshot, err
			}
			item := items[inventoryKey("vm_disk", vm.ID)]
			item.ClaimMiB = vm.ReservedDisk()
			items[inventoryKey("vm_disk", vm.ID)] = item
		}
	}
	addPhysical := func(kind, id, part string, entry storage.NamespaceEntry) error {
		if id == "" {
			return diskError("terminal", "malformed", "physical resource selector is empty")
		}
		physicalParts++
		if physicalParts > storage.MaxNamespaceEntries {
			return diskError("terminal", "size_exceeded", "physical part budget exceeded")
		}
		key := inventoryKey(kind, id)
		item := items[key]
		item.Kind = kind
		item.ID = id
		item.Physical = true
		item.Bytes += entry.Bytes
		capacityRelevant := kind == "volume" || kind == "snapshot" || kind == "vm_disk" || kind == "unknown" || kind == "stage" || kind == "image" && strings.HasPrefix(id, "sha256-")
		if capacityRelevant {
			if !entry.CapacityKnown {
				item.CapacityUnknown = true
			} else {
				item.CapacityMiB = model.SatAdd(item.CapacityMiB, capacityMiB(entry.LogicalBytes))
			}
		}
		if len(item.Parts) >= 16 {
			return diskError("terminal", "size_exceeded", "resource part budget exceeded")
		}
		item.Parts = append(item.Parts, part)
		item.Evidence += entry.Evidence
		if entry.Type == "symlink" {
			item.Problem = "unowned_link"
		}
		items[key] = item
		if len(items) > storage.MaxNamespaceEntries {
			return diskError("terminal", "size_exceeded", "physical inventory exceeds its declared budget")
		}
		return nil
	}
	store := s.runtime.store
	entries, err := storage.InventoryNamespace(ctx, store, s.runtime.runtimeDir)
	if err != nil {
		return snapshot, diskError("unknown", "inventory_unavailable", "runtime namespace inventory unavailable")
	}
	imagePrefix := ""
	if strings.HasPrefix(s.imageDir, s.runtime.runtimeDir+"/") {
		imagePrefix = strings.TrimPrefix(s.imageDir, s.runtime.runtimeDir+"/")
	}
	for _, entry := range entries {
		name := entry.Name
		if name == ".jailer" {
			continue
		}
		if imagePrefix != "" && (name == imagePrefix || strings.HasPrefix(name, imagePrefix+"/")) {
			continue
		}
		first := strings.SplitN(name, "/", 2)[0]
		kind, id := "unknown", first
		switch {
		case strings.Contains(name, "@"):
			before, after, _ := strings.Cut(name, "@")
			kind = "snapshot"
			id = after
			if strings.HasPrefix(before, "clone-") && after == storage.CloneSnapshot {
				kind = "vm_disk"
				id = strings.TrimPrefix(strings.SplitN(before, "/", 2)[0], "clone-")
			}
		case strings.HasPrefix(first, "volume-"):
			kind = "volume"
			id = strings.TrimPrefix(first, "volume-")
		case strings.HasPrefix(first, "checkpoint-"):
			kind = "snapshot"
			id = strings.TrimPrefix(first, "checkpoint-")
		case strings.HasPrefix(first, "clone-"):
			kind = "vm_disk"
			id = strings.TrimPrefix(first, "clone-")
		}
		if store.Name() == "file" {
			v, known := knownVolumes[id]
			if kind == "volume" && known && v.Format == "raw" && v.Engine == "" && entry.Type == "file" {
				entry.LogicalBytes, entry.CapacityKnown = entry.Bytes, true
			} else if entry.Type != "symlink" {
				entry.LogicalBytes, entry.CapacityKnown, err = storage.CapacityEnvelope(ctx, filepath.Join(s.runtime.runtimeDir, name))
				if err != nil {
					return snapshot, diskError("unknown", "inventory_unavailable", "physical capacity could not be established")
				}
			}
		}
		if err = addPhysical(kind, id, "runtime/"+name, entry); err != nil {
			return snapshot, err
		}
	}
	images, err := storage.InventoryNamespace(ctx, store, s.imageDir)
	if err != nil {
		return snapshot, diskError("unknown", "inventory_unavailable", "image namespace inventory unavailable")
	}
	for _, entry := range images {
		id := strings.SplitN(strings.SplitN(entry.Name, "/", 2)[0], "@", 2)[0]
		if store.Name() == "file" && strings.HasPrefix(id, "sha256-") && entry.Type != "symlink" {
			entry.LogicalBytes, entry.CapacityKnown, err = storage.CapacityEnvelope(ctx, filepath.Join(s.imageDir, entry.Name))
			if err != nil {
				return snapshot, diskError("unknown", "inventory_unavailable", "image capacity could not be established")
			}
		}
		if err = addPhysical("image", id, "images/"+entry.Name, entry); err != nil {
			return snapshot, err
		}
	}
	stages, err := storage.InventoryDirectory(ctx, filepath.Join(filepath.Dir(s.dbPath), "archive-stages"), true)
	if err != nil {
		return snapshot, diskError("unknown", "inventory_unavailable", "archive stage inventory unavailable")
	}
	for _, entry := range stages {
		if entry.Type != "symlink" {
			entry.LogicalBytes, entry.CapacityKnown, err = storage.StagingFootprint(ctx, filepath.Join(filepath.Dir(s.dbPath), "archive-stages", entry.Name))
			if err != nil {
				return snapshot, diskError("unknown", "inventory_unavailable", "stage capacity could not be established")
			}
		}
		if err = addPhysical("stage", entry.Name, "archive-stages/"+entry.Name, entry); err != nil {
			return snapshot, err
		}
	}
	for _, engine := range []string{"qemu", "firecracker", "bhyve", "jail", "systemd"} {
		runs, err := storage.InventoryDirectory(ctx, filepath.Join(s.runtime.runDir, engine), true)
		if err != nil {
			return snapshot, diskError("unknown", "inventory_unavailable", "runtime configuration inventory unavailable")
		}
		for _, entry := range runs {
			if err = addPhysical("runtime", entry.Name, "run/"+engine+"/"+entry.Name, entry); err != nil {
				return snapshot, err
			}
		}
	}
	runs, err := storage.InventoryDirectory(ctx, s.runtime.runDir, false)
	if err != nil {
		return snapshot, diskError("unknown", "inventory_unavailable", "runtime marker inventory unavailable")
	}
	for _, entry := range runs {
		if entry.Type == "directory" && (entry.Name == "qemu" || entry.Name == "firecracker" || entry.Name == "bhyve" || entry.Name == "jail" || entry.Name == "systemd") {
			continue
		}
		kind, id := "runtime_unknown", entry.Name
		for _, prefix := range []string{"qemu-", "fc-", "seed-", "ssh-", "bhyve-", "jail-"} {
			if strings.HasPrefix(entry.Name, prefix) {
				kind = "runtime"
				id = strings.TrimPrefix(entry.Name, prefix)
				for _, suffix := range []string{".sandbox.json", ".config.json", ".pid", ".sock", ".iso", ".sh", ".json", ".log"} {
					if strings.HasSuffix(id, suffix) {
						id = strings.TrimSuffix(id, suffix)
						break
					}
				}
				break
			}
		}
		if err = addPhysical(kind, id, "run/"+entry.Name, entry); err != nil {
			return snapshot, err
		}
	}
	scope, err := s.runtime.processScope(ctx)
	if err != nil {
		return snapshot, diskError("unknown", "inventory_unavailable", "process namespace inventory unavailable")
	}
	processes, err := engine.ScopedProcesses(ctx, scope)
	if err != nil {
		return snapshot, diskError("unknown", "inventory_unavailable", "process identities could not be inspected")
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].PID < processes[j].PID })
	processBytes, _ := json.Marshal(processes)
	snapshot.Processes = len(processes)
	snapshot.ProcessDigest = fmt.Sprintf("%x", sha256.Sum256(processBytes))
	// Jails live below the selected physical runtime mount, not the catalog.
	// Enumerate the fixed binary/guest layout without reading guest contents.
	rootPath := s.runtime.runtimeDir
	if store.Name() == "zvol" {
		rootPath, err = storage.NamespaceDirectory(ctx, store, rootPath)
		if err != nil {
			return snapshot, diskError("unknown", "inventory_unavailable", "runtime mount unavailable")
		}
	}
	jailBinaries, err := storage.InventoryDirectory(ctx, filepath.Join(rootPath, ".jailer"), true)
	if err != nil {
		return snapshot, diskError("unknown", "inventory_unavailable", "jail inventory unavailable")
	}
	identities := map[string]string{}
	for _, vm := range vms {
		identities[netcfg.TapName(vm.ID)] = vm.ID
	}
	for _, binary := range jailBinaries {
		if binary.Type != "directory" {
			if err = addPhysical("runtime_unknown", binary.Name, "jailer/"+binary.Name, binary); err != nil {
				return snapshot, err
			}
			continue
		}
		jails, err := storage.InventoryDirectory(ctx, filepath.Join(rootPath, ".jailer", binary.Name), false)
		if err != nil {
			return snapshot, diskError("unknown", "inventory_unavailable", "jail inventory unavailable")
		}
		for _, jail := range jails {
			id := identities[jail.Name]
			if id == "" {
				id = jail.Name
			}
			if err = addPhysical("runtime", id, "jailer/"+binary.Name+"/"+jail.Name, jail); err != nil {
				return snapshot, err
			}
		}
	}
	if store.Name() == "file" {
		checkPayload := func(kind, id, path string, engine model.Engine, size uint32) error {
			key := inventoryKey(kind, id)
			item := items[key]
			if !item.Physical || item.Problem == "unowned_link" {
				return nil
			}
			if engine == model.Firecracker {
				path = filepath.Join(path, "rootfs.ext4")
			}
			if engine == model.Qemu {
				bytes, err := store.QemuSize(ctx, path)
				if err != nil {
					return diskError("unknown", "inventory_unavailable", "disk payload could not be established")
				}
				if bytes < uint64(size)<<20 {
					item.Problem = "payload_size_mismatch"
				}
			} else {
				info, err := os.Lstat(path)
				if os.IsNotExist(err) {
					item.Problem = "payload_missing"
				} else if err != nil {
					return diskError("unknown", "inventory_unavailable", "disk payload could not be inspected")
				} else if !info.Mode().IsRegular() {
					item.Problem = "unowned_link"
				} else if info.Size() < int64(size)<<20 {
					item.Problem = "payload_size_mismatch"
				}
			}
			items[key] = item
			return nil
		}
		for id, v := range knownVolumes {
			if err = checkPayload("volume", id, s.runtime.volumePath(id), v.Engine, v.SizeMiB); err != nil {
				return snapshot, err
			}
		}
		for id, p := range knownPoints {
			if err = checkPayload("snapshot", id, s.runtime.checkpointPath(id), p.Source.Engine, p.Source.SizeMiB); err != nil {
				return snapshot, err
			}
		}
	}
	if store.Name() == "zvol" {
		physical := map[string]storage.NamespaceEntry{}
		for _, entry := range entries {
			physical[entry.Name] = entry
		}
		check := func(kind, id, relative string, engine model.Engine, size uint32) error {
			key := inventoryKey(kind, id)
			item := items[key]
			if !item.Physical {
				return nil
			}
			payload := relative
			if engine == model.Firecracker {
				base, snapshot, point := strings.Cut(relative, "@")
				payload = base + "/rootfs"
				if point {
					payload += "@" + snapshot
				}
			}
			entry, exists := physical[payload]
			if !exists {
				item.Problem = "payload_missing"
			} else if !entry.CapacityKnown {
				item.Problem = "payload_unconfirmed"
			} else if entry.LogicalBytes < uint64(size)<<20 {
				item.Problem = "payload_size_mismatch"
			}
			if engine == model.Firecracker && exists {
				directory, err := storage.NamespaceDirectory(ctx, store, s.runtime.runtimeDir+"/"+relative)
				if err != nil {
					return diskError("unknown", "inventory_unavailable", "checkpoint namespace unavailable")
				}
				info, err := os.Lstat(filepath.Join(directory, "vmlinux"))
				if os.IsNotExist(err) {
					item.Problem = "payload_missing"
				} else if err != nil {
					return diskError("unknown", "inventory_unavailable", "kernel payload unavailable")
				} else if !info.Mode().IsRegular() {
					item.Problem = "unowned_link"
				}
			}
			items[key] = item
			return nil
		}
		for id, v := range knownVolumes {
			if err = check("volume", id, "volume-"+id, v.Engine, v.SizeMiB); err != nil {
				return snapshot, err
			}
		}
		for id, p := range knownPoints {
			if err = check("snapshot", id, "volume-"+p.VolumeID+"@"+id, p.Source.Engine, p.Source.SizeMiB); err != nil {
				return snapshot, err
			}
		}
	}
	var after uint64
	if err = db.conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&after); err != nil {
		return snapshot, err
	}
	current, err := s.commandJournal.ControlReport()
	if err != nil {
		return snapshot, err
	}
	if before != after || current.Authority != control.Authority || current.Epoch != control.Epoch || current.Phase != control.Phase {
		return snapshot, diskError("state", "unstable_inventory", "inventory changed during capture")
	}
	for _, item := range items {
		sort.Strings(item.Parts)
		item.Evidence = fmt.Sprintf("%x", sha256.Sum256([]byte(item.Evidence+item.Kind+item.ID+item.State)))
		snapshot.Items = append(snapshot.Items, item)
		if item.CapacityUnknown {
			snapshot.QuarantinePinned = true
		}
		snapshot.QuarantineMiB = model.SatAdd(snapshot.QuarantineMiB, model.SatSub(item.CapacityMiB, item.ClaimMiB))
	}
	sort.Slice(snapshot.Items, func(i, j int) bool {
		return inventoryKey(snapshot.Items[i].Kind, snapshot.Items[i].ID) < inventoryKey(snapshot.Items[j].Kind, snapshot.Items[j].ID)
	})
	raw, _ := json.Marshal(snapshot.Items)
	snapshot.Digest = fmt.Sprintf("%x", sha256.Sum256(raw))
	snapshot.Total = len(snapshot.Items)
	snapshot.Complete = true
	return snapshot, nil
}
func (s *Server) physicalInventoryHTTP(w http.ResponseWriter, r *http.Request) {
	size := 100
	if raw := r.URL.Query().Get("size"); raw != "" {
		var err error
		size, err = strconv.Atoi(raw)
		if err != nil || size < 1 || size > 100 {
			failure(w, diskError("terminal", "invalid_page", "inventory page size must be within 1..100"))
			return
		}
	}
	offset := 0
	if raw := r.URL.Query().Get("after"); raw != "" {
		var err error
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			failure(w, diskError("terminal", "invalid_page", "invalid inventory cursor"))
			return
		}
	}
	id := r.URL.Query().Get("snapshot")
	s.inventoryMu.Lock()
	cached := s.inventory
	if id != "" {
		if cached == nil || cached.snapshot.ID != id || time.Now().After(cached.expires) {
			s.inventoryMu.Unlock()
			failure(w, diskError("state", "inventory_expired", "inventory cursor must restart"))
			return
		}
		snapshot := cached.snapshot
		current, err := s.commandJournal.ControlReport()
		if err != nil || current.Authority != snapshot.Authority || current.Epoch != snapshot.Epoch {
			s.inventoryMu.Unlock()
			failure(w, diskError("state", "inventory_expired", "inventory epoch changed"))
			return
		}
		s.inventoryMu.Unlock()
		inventoryPage(w, snapshot, offset, size)
		return
	}
	s.inventoryMu.Unlock()
	if offset != 0 {
		failure(w, diskError("terminal", "invalid_page", "cursor requires a snapshot identity"))
		return
	}
	select {
	case s.inventoryLane <- struct{}{}:
		defer func() { <-s.inventoryLane }()
	default:
		failure(w, diskError("rate_limited", "inventory_busy", "inventory lane is occupied"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	snapshot, err := s.physicalInventory(ctx)
	if err != nil {
		var typed *api.BoundaryError
		if !errors.As(err, &typed) {
			err = diskError("unknown", "inventory_unavailable", "physical inventory unavailable")
		}
		failure(w, err)
		return
	}
	s.inventoryMu.Lock()
	s.inventory = &inventoryCache{snapshot: snapshot, expires: time.Now().Add(time.Minute)}
	s.inventoryMu.Unlock()
	inventoryPage(w, snapshot, 0, size)
}
func inventoryPage(w http.ResponseWriter, snapshot model.PhysicalInventory, offset, size int) {
	if offset > len(snapshot.Items) {
		failure(w, diskError("terminal", "invalid_page", "cursor exceeds inventory"))
		return
	}
	end := min(offset+size, len(snapshot.Items))
	snapshot.Items = snapshot.Items[offset:end]
	if end < snapshot.Total {
		snapshot.NextToken = strconv.Itoa(end)
	}
	api.WriteJSON(w, 200, api.Success(snapshot))
}
