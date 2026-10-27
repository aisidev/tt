package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/internal/storage"
	"github.com/aisidev/tt/model"
)

const diskSchema = `CREATE TABLE volumes(id TEXT PRIMARY KEY,data TEXT NOT NULL);
CREATE TABLE volume_snapshots(id TEXT PRIMARY KEY,data TEXT NOT NULL);`

func diskError(class, code, message string) error {
	return &api.BoundaryError{Class: class, Code: code, Message: message}
}
func diskRecord[T any](ctx context.Context, q sqlQuery, table, id string) (*T, error) {
	var raw sqlText
	err := q.QueryRowContext(ctx, "SELECT data FROM "+table+" WHERE id=?1", id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, diskError("transient", "unavailable", "disk catalog unavailable")
	}
	var record T
	if json.Unmarshal([]byte(raw), &record) != nil {
		return nil, diskError("terminal", "malformed", "malformed disk catalog record")
	}
	switch v := any(&record).(type) {
	case *model.Volume:
		if v.ID != id || v.Generation == 0 || v.SizeMiB == 0 || v.State == "" || v.Phase == "" {
			return nil, diskError("terminal", "malformed", "malformed volume record")
		}
	case *model.VolumeSnapshot:
		if v.ID != id || v.Generation == 0 || v.Source.ID == "" || v.State == "" {
			return nil, diskError("terminal", "malformed", "malformed checkpoint record")
		}
	}
	return &record, nil
}
func saveDisk(ctx context.Context, q sqlQuery, table, id string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, "INSERT INTO "+table+"(id,data) VALUES(?1,?2) ON CONFLICT(id) DO UPDATE SET data=excluded.data", id, string(raw))
	return err
}
func diskList[T any](ctx context.Context, q sqlQuery, table string) ([]T, error) {
	rows, err := q.QueryContext(ctx, "SELECT id FROM "+table+" ORDER BY id")
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id sqlText
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, string(id))
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []T{}
	for _, id := range ids {
		v, err := diskRecord[T](ctx, q, table, id)
		if err != nil {
			return nil, err
		}
		if v == nil {
			return nil, diskError("transient", "unavailable", "disk catalog changed during read")
		}
		out = append(out, *v)
	}
	return out, nil
}
func (r *Runtime) volumePath(id string) string     { return r.runtimeDir + "/volume-" + id }
func (r *Runtime) checkpointPath(id string) string { return r.runtimeDir + "/checkpoint-" + id }
func (r *Runtime) volume(ctx context.Context, id string) (*model.Volume, error) {
	v, err := diskRecord[model.Volume](ctx, r.db.conn, "volumes", id)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, diskError("terminal", "not_found", "volume not found")
	}
	return v, nil
}
func (r *Runtime) saveVolume(ctx context.Context, v *model.Volume) error {
	v.Revision++
	return saveDisk(ctx, r.db.conn, "volumes", v.ID, v)
}
func (r *Runtime) saveSnapshot(ctx context.Context, v *model.VolumeSnapshot) error {
	v.Revision++
	return saveDisk(ctx, r.db.conn, "volume_snapshots", v.ID, v)
}

func (r *Runtime) CreateVolume(ctx context.Context, spec model.VolumeSpec, claim ...*model.Vm) (*model.Volume, error) {
	if model.ValidateName(spec.ID, "volume") != nil || spec.SizeMiB == 0 || spec.SizeMiB > 1<<20 || spec.Generation == 0 || !strings.HasSuffix(spec.ID, fmt.Sprintf("-g%d", spec.Generation)) {
		return nil, diskError("terminal", "invalid_volume", "invalid volume identity or size")
	}
	if spec.HostID != "" && spec.HostID != r.hostID {
		return nil, diskError("state", "wrong_locality", "volume belongs to another host")
	}
	if spec.Engine != "" && spec.Engine != model.Firecracker && spec.Engine != model.Qemu {
		return nil, diskError("terminal", "unsupported_engine", "root volume requires QEMU or Firecracker")
	}
	if (spec.Image != "") != (spec.Engine != "") {
		return nil, diskError("terminal", "invalid_volume", "root image and engine are required together")
	}
	if spec.Image != "" && model.ValidateImage(spec.Image, spec.Engine) != nil {
		return nil, diskError("terminal", "invalid_volume", "invalid source image")
	}
	if spec.FromSnapshot == "" && spec.Image != "" {
		if err := r.imageImportGuard(ctx, spec.Image); err != nil {
			return nil, err
		}
	}
	var source *model.VolumeSnapshot
	if spec.FromSnapshot != "" {
		var err error
		source, err = diskRecord[model.VolumeSnapshot](ctx, r.db.conn, "volume_snapshots", spec.FromSnapshot)
		if err != nil {
			return nil, err
		}
		if source == nil || source.State != "ready" {
			return nil, diskError("state", "snapshot_unavailable", "source checkpoint is not ready")
		}
		if spec.SizeMiB < source.Source.SizeMiB {
			return nil, diskError("terminal", "volume_shrink", "restored volume cannot shrink")
		}
		spec.Engine = source.Source.Engine
		spec.Image = source.Source.Image
	}
	existing, err := diskRecord[model.Volume](ctx, r.db.conn, "volumes", spec.ID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.Generation != spec.Generation || existing.Image != spec.Image || existing.Engine != spec.Engine || existing.Owner != spec.Owner || existing.SizeMiB != spec.SizeMiB || existing.SourceSnapshot != spec.FromSnapshot {
			return nil, diskError("terminal", "fingerprint_conflict", "volume identity has a different creation intent")
		}
		if existing.State == "ready" || existing.State == "attached" {
			return existing, nil
		}
		if existing.Phase == "archive" {
			return nil, diskError("state", "volume_busy", "volume belongs to an archive import")
		}
		if existing.State != "creating" {
			return nil, diskError("state", "volume_busy", "volume is unavailable")
		}
	} else {
		format := "raw"
		if spec.Engine == model.Qemu {
			format = r.store.DiskFormat()
		}
		existing = &model.Volume{ID: spec.ID, Owner: spec.Owner, HostID: r.hostID, Generation: spec.Generation, SizeMiB: spec.SizeMiB, Engine: spec.Engine, Image: spec.Image, Format: format, Retain: spec.Retain, State: "creating", Phase: "reserved", SourceSnapshot: spec.FromSnapshot, CreatedAt: r.now()}
		if err = r.recount(ctx); err != nil {
			return nil, err
		}
		credit := uint32(0)
		if len(claim) > 0 && claim[0].Options.RootVolumeID == spec.ID {
			credit = claim[0].Options.RootDiskCharge
		}
		if !r.resource.CanFit(0, 0, model.SatSub(existing.ReservedMiB(), credit)) {
			return nil, diskError("transient", "capacity_exceeded", "insufficient disk capacity")
		}
		if len(claim) > 0 && claim[0].Options.RootVolumeID == spec.ID {
			tx, e := r.db.conn.BeginTx(ctx, nil)
			if e != nil {
				return nil, e
			}
			defer tx.Rollback()
			existing.Revision++
			if e = saveDisk(ctx, tx, "volumes", existing.ID, existing); e != nil {
				return nil, e
			}
			claim[0].Options.RootDiskCharge = 0
			if e = saveVM(ctx, tx, claim[0]); e != nil {
				return nil, e
			}
			if e = tx.Commit(); e != nil {
				return nil, e
			}
		} else if err = r.saveVolume(ctx, existing); err != nil {
			return nil, err
		}
	}
	path := r.volumePath(spec.ID)
	// The journal proves this disk was never published ready or attached. A
	// retry may discard this owned, unpublished stage, never a ready user disk.
	if existing.Phase == "copying" {
		if err = r.store.RemoveImage(ctx, path); err != nil {
			return nil, err
		}
	}
	existing.Phase = "copying"
	if err = r.saveVolume(ctx, existing); err != nil {
		return nil, err
	}
	if source != nil {
		err = storage.CloneVolumeSnapshot(ctx, r.store, r.volumePath(source.VolumeID), r.checkpointPath(source.ID), path, source.ID, source.Source.Engine)
	} else if spec.Image != "" {
		var sourcePath string
		sourcePath, err = storage.VolumeSource(ctx, r.store, r.imageDir+"/"+spec.Image, spec.Engine)
		if err == nil {
			err = r.store.CloneImage(ctx, sourcePath, path)
		}
	} else {
		err = storage.CreateRawVolume(ctx, r.store, path, spec.SizeMiB)
	}
	if err == nil {
		err = r.growVolume(ctx, existing)
	}
	if err == nil {
		err = storage.SyncVolume(ctx, r.store, path, spec.Engine)
	}
	if err != nil {
		existing.Error = stringPointer("volume creation incomplete")
		_ = r.saveVolume(ctx, existing)
		return nil, diskError("unknown", "volume_incomplete", "volume creation remains retained for retry")
	}
	existing.State = "ready"
	existing.Phase = "ready"
	existing.Error = nil
	if err = r.saveVolume(ctx, existing); err != nil {
		return nil, err
	}
	if err = r.recount(ctx); err != nil {
		return nil, err
	}
	return existing, nil
}
func (r *Runtime) growVolume(ctx context.Context, v *model.Volume) error {
	path := r.volumePath(v.ID)
	switch v.Engine {
	case model.Firecracker:
		return r.store.ResizeFirecracker(ctx, path, v.SizeMiB)
	case model.Qemu:
		return r.store.ResizeDisk(ctx, path, v.SizeMiB)
	default:
		return storage.GrowRawVolume(ctx, r.store, path, v.SizeMiB)
	}
}
func (r *Runtime) UpdateVolume(ctx context.Context, id string, update model.VolumeUpdate) (*model.Volume, error) {
	if err := r.archiveGuard(id); err != nil {
		return nil, err
	}
	v, err := r.volume(ctx, id)
	if err != nil {
		return nil, err
	}
	if v.State != "ready" && v.State != "attached" && v.State != "growing" {
		return nil, diskError("state", "volume_busy", "volume has unsettled work")
	}
	if v.PendingGroup != "" {
		return nil, diskError("state", "checkpoint_busy", "workspace checkpoint fences disk mutation")
	}
	if v.PendingSnapshot != "" {
		return nil, diskError("state", "volume_busy", "checkpoint writer is unsettled")
	}
	if update.SizeMiB != nil {
		target := *update.SizeMiB
		if target < v.SizeMiB || target == 0 || target > 1<<20 {
			return nil, diskError("terminal", "volume_shrink", "volume size must grow within the supported range")
		}
		if v.State == "growing" && target != v.SizeMiB {
			return nil, diskError("state", "volume_busy", "retry the recorded growth before another update")
		}
		if err = r.requireStoppedVolume(ctx, v); err != nil {
			return nil, err
		}
		before := v.ReservedMiB()
		v.SizeMiB = target
		if err = r.recount(ctx); err != nil {
			return nil, err
		}
		if !r.resource.CanFit(0, 0, model.SatSub(v.ReservedMiB(), before)) {
			return nil, diskError("transient", "capacity_exceeded", "insufficient disk capacity")
		}
		v.State = "growing"
		v.Phase = "growing"
		if err = r.saveVolume(ctx, v); err != nil {
			return nil, err
		}
		if err = r.growVolume(ctx, v); err != nil {
			return nil, diskError("unknown", "volume_incomplete", "volume growth retained for retry")
		}
	}
	if update.Retain != nil {
		v.Retain = *update.Retain
	}
	v.State = "ready"
	if v.AttachedTo != "" {
		v.State = "attached"
	}
	v.Phase = "ready"
	v.Error = nil
	if v.AttachedTo != "" && update.SizeMiB != nil {
		vm, e := r.requireVM(ctx, v.AttachedTo)
		if e != nil {
			return nil, e
		}
		if v.Role == "root" {
			vm.Disk = model.SatAdd(v.SizeMiB, vm.Options.ConfigDiskMiB(vm.Engine))
			vm.Options.RequestedDisk = v.SizeMiB
		} else {
			for index := range vm.Options.DataVolumes {
				if vm.Options.DataVolumes[index].ID == v.ID {
					vm.Options.DataVolumes[index].SizeMiB = v.SizeMiB
				}
			}
		}
		tx, e := r.db.conn.BeginTx(ctx, nil)
		if e != nil {
			return nil, e
		}
		defer tx.Rollback()
		v.Revision++
		if e = saveDisk(ctx, tx, "volumes", v.ID, v); e != nil {
			return nil, e
		}
		if e = saveVM(ctx, tx, vm); e != nil {
			return nil, e
		}
		if e = tx.Commit(); e != nil {
			return nil, e
		}
	} else if err = r.saveVolume(ctx, v); err != nil {
		return nil, err
	}
	return v, r.recount(ctx)
}
func (r *Runtime) requireStoppedVolume(ctx context.Context, v *model.Volume) error {
	if v.AttachedTo == "" {
		return nil
	}
	vm, err := r.requireVM(ctx, v.AttachedTo)
	if err != nil {
		return err
	}
	if vm.State != model.VmStopped {
		return diskError("state", "workspace_not_stopped", "disk operations require a stopped VM")
	}
	eng, err := r.engineFactory(vm.Engine, r.containerRuntime)
	if err != nil {
		return err
	}
	state, err := eng.State(ctx, vm)
	if err != nil {
		return err
	}
	if state != model.VmStopped {
		return diskError("state", "workspace_not_stopped", "engine has not confirmed stop")
	}
	return nil
}
func (r *Runtime) DeleteVolume(ctx context.Context, id string) error {
	if err := r.archiveGuard(id); err != nil {
		return err
	}
	v, err := r.volume(ctx, id)
	if err != nil {
		var typed *api.BoundaryError
		if errors.As(err, &typed) && typed.Class == "terminal" && typed.Code == "not_found" {
			return r.confirmUnadmittedVolumeAbsent(ctx, id)
		}
		return err
	}
	if v.State == "deleted" {
		return nil
	}
	if v.AttachedTo != "" {
		return diskError("state", "volume_attached", "detach the volume before destruction")
	}
	if v.PendingGroup != "" {
		return diskError("state", "checkpoint_busy", "workspace checkpoint fences disk destruction")
	}
	if v.Retain {
		return diskError("state", "volume_retained", "clear retention explicitly before destruction")
	}
	points, err := diskList[model.VolumeSnapshot](ctx, r.db.conn, "volume_snapshots")
	if err != nil {
		return err
	}
	for _, p := range points {
		if p.VolumeID == id && p.State != "deleted" {
			return diskError("state", "snapshot_dependency", "checkpoint still depends on this volume")
		}
	}
	v.State = "deleting"
	v.Phase = "deleting"
	if err = r.saveVolume(ctx, v); err != nil {
		return err
	}
	if err = r.store.RemoveImage(ctx, r.volumePath(id)); err != nil {
		return diskError("unknown", "volume_incomplete", "disk destruction remains retained for retry")
	}
	if err = r.deleteArchiveStage(ctx, id); err != nil {
		return err
	}
	v.State = "deleted"
	v.Phase = "deleted"
	if err = r.saveVolume(ctx, v); err != nil {
		return err
	}
	return r.recount(ctx)
}
func (r *Runtime) SnapshotVolume(ctx context.Context, id, snapshotID string) (*model.VolumeSnapshot, error) {
	if model.ValidateName(snapshotID, "snapshot") != nil {
		return nil, diskError("terminal", "invalid_snapshot", "invalid checkpoint identity")
	}
	v, err := r.volume(ctx, id)
	if err != nil {
		return nil, err
	}
	if v.State != "ready" && v.State != "attached" {
		return nil, diskError("state", "volume_busy", "volume has unsettled work")
	}
	if err = r.checkpointWriter(ctx, v); err != nil {
		return nil, err
	}
	if v.PendingSnapshot != "" && v.PendingSnapshot != snapshotID {
		return nil, diskError("state", "volume_busy", "another checkpoint writer is unsettled")
	}

	if err = r.requireStoppedVolume(ctx, v); err != nil {
		return nil, err
	}
	point, err := diskRecord[model.VolumeSnapshot](ctx, r.db.conn, "volume_snapshots", snapshotID)
	if err != nil {
		return nil, err
	}
	if point != nil {
		if point.VolumeID != id {
			return nil, diskError("terminal", "fingerprint_conflict", "checkpoint identity belongs to a different volume")
		}
		if point.State == "ready" {
			if v.PendingSnapshot == snapshotID {
				v.PendingSnapshot = ""
				if err = r.saveVolume(ctx, v); err != nil {
					return nil, err
				}
			}
			return point, nil
		}
		if point.State != "creating" {
			return nil, diskError("state", "snapshot_unavailable", "checkpoint is unavailable")
		}
	} else {
		if err = r.recount(ctx); err != nil {
			return nil, err
		}
		if !r.resource.CanFit(0, 0, v.ReservedMiB()) {
			return nil, diskError("transient", "capacity_exceeded", "insufficient checkpoint capacity")
		}
		point = &model.VolumeSnapshot{ID: snapshotID, VolumeID: id, Owner: v.Owner, HostID: r.hostID, Generation: v.Generation, State: "creating", Phase: "reserved", Source: *v, CreatedAt: r.now()}
		if err = r.saveSnapshot(ctx, point); err != nil {
			return nil, err
		}
	}
	v.PendingSnapshot = snapshotID
	if err = r.saveVolume(ctx, v); err != nil {
		return nil, err
	}
	if point.Phase == "copying" {
		if err = storage.RemoveVolumeSnapshot(ctx, r.store, r.volumePath(id), r.checkpointPath(snapshotID), snapshotID, v.Engine); err != nil {
			return nil, err
		}
	}
	point.Phase = "copying"
	if err = r.saveSnapshot(ctx, point); err != nil {
		return nil, err
	}
	if err = storage.SnapshotVolume(ctx, r.store, r.volumePath(id), r.checkpointPath(snapshotID), snapshotID, v.Engine); err != nil {
		return nil, diskError("unknown", "snapshot_incomplete", "checkpoint remains retained for retry")
	}
	point.State = "ready"
	point.Phase = "ready"
	if err = r.saveSnapshot(ctx, point); err != nil {
		return nil, err
	}
	v.PendingSnapshot = ""
	if err = r.saveVolume(ctx, v); err != nil {
		return nil, err
	}
	return point, r.recount(ctx)
}
func (r *Runtime) DeleteVolumeSnapshot(ctx context.Context, id string) error {
	p, err := diskRecord[model.VolumeSnapshot](ctx, r.db.conn, "volume_snapshots", id)
	if err != nil {
		return err
	}
	if p == nil {
		return diskError("terminal", "not_found", "checkpoint not found")
	}
	if p.State == "deleted" {
		return r.clearSnapshotWriter(ctx, p)
	}
	volumes, err := diskList[model.Volume](ctx, r.db.conn, "volumes")
	if err != nil {
		return err
	}
	for _, v := range volumes {
		if v.SourceSnapshot == id && v.State != "deleted" {
			return diskError("state", "snapshot_dependency", "restored volume still depends on this checkpoint")
		}
	}
	p.State = "deleting"
	p.Phase = "deleting"
	if err = r.saveSnapshot(ctx, p); err != nil {
		return err
	}
	if err = storage.RemoveVolumeSnapshot(ctx, r.store, r.volumePath(p.VolumeID), r.checkpointPath(id), id, p.Source.Engine); err != nil {
		return diskError("unknown", "snapshot_incomplete", "checkpoint destruction remains retained for retry")
	}
	p.State = "deleted"
	p.Phase = "deleted"
	if err = r.saveSnapshot(ctx, p); err != nil {
		return err
	}
	if err = r.clearSnapshotWriter(ctx, p); err != nil {
		return err
	}
	return r.recount(ctx)
}

// honorVolumeFence joins a VM's attachment effect to the volume's receiver
// watermark. A late independent disk command cannot alter a newer binding.
func (r *Runtime) honorVolumeFence(ctx context.Context, id string) error {
	envelope, ok := command.FromContext(ctx)
	if !ok {
		return diskError("terminal", "missing_identity", "managed disks require command identity")
	}
	var highest uint64
	if err := r.db.conn.QueryRowContext(ctx, "SELECT COALESCE((SELECT token FROM command_fences WHERE subject=?1),0)", id).Scan(&highest); err != nil {
		return err
	}
	if envelope.Token < highest {
		return diskError("state", "stale_fence", "disk ownership was superseded")
	}
	_, err := r.db.conn.ExecContext(ctx, "INSERT INTO command_fences(subject,token) VALUES(?1,?2) ON CONFLICT(subject) DO UPDATE SET token=excluded.token", id, envelope.Token)
	return err
}

func diskUsage(ctx context.Context, q sqlQuery) (uint32, error) {
	volumes, err := diskList[model.Volume](ctx, q, "volumes")
	if err != nil {
		return 0, err
	}
	snapshots, err := diskList[model.VolumeSnapshot](ctx, q, "volume_snapshots")
	if err != nil {
		return 0, err
	}
	var total uint32
	for _, v := range volumes {
		total = model.SatAdd(total, v.ReservedMiB())
	}
	for _, p := range snapshots {
		if p.State != "deleted" {
			total = model.SatAdd(total, p.Source.ReservedMiB())
		}
	}
	scratch, err := archiveUsage(ctx, q)
	if err != nil {
		return 0, err
	}
	total = model.SatAdd(total, scratch)
	return total, nil
}
func ReadDiskUsage(ctx context.Context, path string) (uint32, error) {
	db, err := openDatabase(ctx, path, true, 2000)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	return diskUsage(ctx, db.conn)
}

func (r *Runtime) clearSnapshotWriter(ctx context.Context, p *model.VolumeSnapshot) error {
	v, err := r.volume(ctx, p.VolumeID)
	if err != nil {
		return err
	}
	if v.PendingSnapshot == p.ID {
		v.PendingSnapshot = ""
		return r.saveVolume(ctx, v)
	}
	return nil
}

// A missing metadata row is not absence. This fenced delete acknowledges only
// after ruling out live bindings and confirming physical absence; unowned bytes
// remain untouched for explicit resolution.
func (r *Runtime) confirmUnadmittedVolumeAbsent(ctx context.Context, id string) error {
	if model.ValidateName(id, "volume") != nil {
		return diskError("terminal", "invalid_volume", "invalid volume identity")
	}
	vms, err := r.ListVMs(ctx)
	if err != nil {
		return err
	}
	for _, vm := range vms {
		if vm.Options.RootVolumeID == id {
			return diskError("unknown", "volume_unowned", "a VM still references the missing disk record")
		}
		for _, binding := range vm.Options.DataVolumes {
			if binding.ID == id {
				return diskError("unknown", "volume_unowned", "a VM still references the missing disk record")
			}
		}
	}
	exists := false
	if r.storage == model.File {
		_, err = os.Lstat(r.volumePath(id))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err == nil {
			exists = true
		}
	} else {
		exists, err = r.store.ImageExists(ctx, r.volumePath(id))
	}
	if err != nil {
		return diskError("unknown", "volume_unavailable", "physical disk absence is unavailable")
	}
	if exists {
		return diskError("unknown", "volume_unowned", "unowned disk bytes require explicit resolution")
	}
	return nil
}
