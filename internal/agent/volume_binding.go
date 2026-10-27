package agent

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/storage"
	"github.com/aisidev/tt/model"
)

func volumeGeneration(id string) uint64 {
	index := strings.LastIndex(id, "-g")
	if index < 0 {
		return 0
	}
	n, _ := strconv.ParseUint(id[index+2:], 10, 64)
	return n
}
func (r *Runtime) dataAttachment(ctx context.Context, id string) (model.DiskAttachment, error) {
	v, err := r.volume(ctx, id)
	if err != nil {
		return model.DiskAttachment{}, err
	}
	if v.Engine != "" || v.Image != "" || v.Format != "raw" {
		return model.DiskAttachment{}, diskError("terminal", "invalid_volume_role", "data attachment requires a raw data volume")
	}
	if v.State != "ready" && v.State != "attached" || (v.PendingSnapshot != "" || v.PendingGroup != "") {
		return model.DiskAttachment{}, diskError("state", "volume_busy", "data volume is unsettled")
	}
	path := r.volumePath(id)
	if r.storage == model.Zvol {
		path, err = storage.ResolveVolumeDevice(ctx, r.store, path)
	} else {
		var info os.FileInfo
		info, err = os.Lstat(path)
		if err == nil && (!info.Mode().IsRegular() || info.Size() < int64(v.SizeMiB)*1024*1024) {
			err = fmt.Errorf("data disk is missing or smaller than its declared capacity")
		}
	}
	if err != nil {
		return model.DiskAttachment{}, diskError("unknown", "volume_missing", "data disk cannot be established; retained")
	}
	return model.DiskAttachment{ID: id, Path: path, Format: v.Format, SizeMiB: v.SizeMiB}, nil
}
func (r *Runtime) validateManagedBindings(ctx context.Context, vm *model.Vm) error {
	if vm.Options.RootManaged && vm.Options.RootVolumeID == "" {
		return diskError("state", "root_detached", "attach a root volume before starting")
	}
	ids := []string{}
	if vm.Options.RootManaged {
		ids = append(ids, vm.Options.RootVolumeID)
	}
	for _, d := range vm.Options.DataVolumes {
		ids = append(ids, d.ID)
	}
	for _, id := range ids {
		v, err := r.volume(ctx, id)
		if err != nil {
			return err
		}
		if v.AttachedTo != vm.ID || v.State != "attached" || (v.PendingSnapshot != "" || v.PendingGroup != "") {
			return diskError("state", "volume_busy", "volume binding or checkpoint is unsettled")
		}
	}
	return nil
}
func (r *Runtime) AttachVolume(ctx context.Context, id string, binding model.VolumeBinding) error {
	vm, err := r.requireVM(ctx, binding.VMID)
	if err != nil {
		return err
	}
	v, err := r.volume(ctx, id)
	if err != nil {
		return err
	}
	return r.bindVolume(ctx, vm, v, binding.Role, false)
}
func (r *Runtime) bindVolume(ctx context.Context, vm *model.Vm, v *model.Volume, role string, creating bool) error {
	if err := r.archiveGuard(v.ID); err != nil {
		return err
	}
	if err := r.honorVolumeFence(ctx, v.ID); err != nil {
		return err
	}
	if role != "root" && role != "data" {
		return diskError("terminal", "invalid_volume_role", "volume role must be root or data")
	}
	if v.HostID != r.hostID || vm.HostID != r.hostID {
		return diskError("state", "wrong_locality", "disk and VM locality must match")
	}
	if v.State != "ready" && v.State != "attached" || (v.PendingSnapshot != "" || v.PendingGroup != "") {
		return diskError("state", "volume_busy", "volume is unsettled")
	}
	if v.AttachedTo != "" && (v.AttachedTo != vm.ID || v.Role != role) {
		return diskError("state", "volume_attached", "volume is already attached")
	}
	if vm.State != model.VmStopped && !(creating && vm.State == model.VmCreating) {
		return diskError("state", "workspace_not_stopped", "attachment is offline only")
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
	if vm.Backup.HasArtifacts() {
		return diskError("state", "backup_dependency", "legacy recovery points prevent attachment changes")
	}
	switch role {
	case "root":
		if !vm.Options.RootManaged || vm.Options.RootVolumeID != "" && vm.Options.RootVolumeID != v.ID {
			return diskError("state", "root_attached", "detach the previous managed root first")
		}
		if v.Engine != vm.Engine || v.Image == "" || v.Image != vm.Image {
			return diskError("terminal", "invalid_volume_role", "root image and engine must match the VM")
		}
		vm.Options.RootVolumeID = v.ID
		vm.Options.RootDiskCharge = 0
		vm.Disk = model.SatAdd(v.SizeMiB, vm.Options.ConfigDiskMiB(vm.Engine))
	case "data":
		attachment, err := r.dataAttachment(ctx, v.ID)
		if err != nil {
			return err
		}
		found := false
		for _, disk := range vm.Options.DataVolumes {
			if disk.ID == v.ID {
				found = true
			}
		}
		if !found {
			if len(vm.Options.DataVolumes) >= 8 {
				return diskError("terminal", "size_exceeded", "a VM supports at most eight data disks")
			}
			vm.Options.DataVolumes = append(vm.Options.DataVolumes, attachment)
		}
	}
	v.AttachedTo = vm.ID
	v.AttachedEnv = vm.EnvID
	v.Role = role
	v.State = "attached"
	tx, err := r.db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = saveVM(ctx, tx, vm); err != nil {
		return err
	}
	v.Revision++
	if err = saveDisk(ctx, tx, "volumes", v.ID, v); err != nil {
		return err
	}
	return tx.Commit()
}
func (r *Runtime) DetachVolume(ctx context.Context, id, vmID string) error {
	if err := r.archiveGuard(id); err != nil {
		return err
	}
	if err := r.honorVolumeFence(ctx, id); err != nil {
		return err
	}
	v, err := r.volume(ctx, id)
	if err != nil {
		return err
	}
	vm, err := r.requireVM(ctx, vmID)
	if err != nil {
		return err
	}
	if v.AttachedTo == "" {
		return nil
	}
	if v.AttachedTo != vmID {
		return diskError("state", "volume_attached", "volume belongs to another VM")
	}
	if err = r.requireStoppedVolume(ctx, v); err != nil {
		return err
	}
	if v.PendingSnapshot != "" || v.PendingGroup != "" {
		return diskError("state", "volume_busy", "checkpoint writer is unsettled")
	}
	if v.Role == "root" {
		vm.Options.RootVolumeID = ""
		vm.Options.RootDiskCharge = 0
	} else {
		vm.Options.DataVolumes = slices.DeleteFunc(vm.Options.DataVolumes, func(d model.DiskAttachment) bool { return d.ID == id })
	}
	v.AttachedTo = ""
	v.AttachedEnv = ""
	v.Role = ""
	v.State = "ready"
	tx, err := r.db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = saveVM(ctx, tx, vm); err != nil {
		return err
	}
	v.Revision++
	if err = saveDisk(ctx, tx, "volumes", id, v); err != nil {
		return err
	}
	return tx.Commit()
}
func (r *Runtime) prepareManagedVolumes(ctx context.Context, vm *model.Vm, request api.CreateVmReq) error {
	if vm.Options.RootManaged {
		root, err := diskRecord[model.Volume](ctx, r.db.conn, "volumes", vm.Options.RootVolumeID)
		if err != nil {
			return err
		}
		if root == nil {
			if !request.DurableRoot {
				return diskError("terminal", "not_found", "root volume does not exist")
			}
			retain := true
			if request.RootRetain != nil {
				retain = *request.RootRetain
			}
			owner := request.VolumeOwner
			if owner == "" {
				owner = request.EnvID
			}
			root, err = r.CreateVolume(ctx, model.VolumeSpec{ID: vm.Options.RootVolumeID, HostID: r.hostID, Owner: owner, Generation: volumeGeneration(vm.Options.RootVolumeID), Engine: vm.Engine, Image: vm.Image, SizeMiB: request.Disk, Retain: retain}, vm)
			if err != nil {
				return err
			}
		} else if root.State == "creating" {
			if !request.DurableRoot {
				return diskError("state", "volume_busy", "root volume creation is unsettled")
			}
			root, err = r.CreateVolume(ctx, model.VolumeSpec{ID: root.ID, Owner: root.Owner, Generation: root.Generation, Engine: root.Engine, Image: root.Image, SizeMiB: root.SizeMiB, Retain: root.Retain, FromSnapshot: root.SourceSnapshot}, vm)
			if err != nil {
				return err
			}
		}
		if err = r.bindVolume(ctx, vm, root, "root", true); err != nil {
			return err
		}
	}
	for _, id := range request.DataVolumeIDs {
		v, err := r.volume(ctx, id)
		if err != nil {
			return err
		}
		if err = r.bindVolume(ctx, vm, v, "data", true); err != nil {
			return err
		}
	}
	return nil
}

// Detachment on VM deletion preserves every managed disk. Retention-aware
// destruction is a separate explicit volume operation, after dependency checks.
func (r *Runtime) detachDeletedVM(ctx context.Context, vm *model.Vm) error {
	volumes, err := diskList[model.Volume](ctx, r.db.conn, "volumes")
	if err != nil {
		return err
	}
	for i := range volumes {
		v := &volumes[i]
		if v.AttachedTo != vm.ID {
			continue
		}
		if v.PendingSnapshot != "" || v.PendingGroup != "" {
			return diskError("state", "volume_busy", "checkpoint writer is unsettled")
		}
		if err = r.honorVolumeFence(ctx, v.ID); err != nil {
			return err
		}
		v.AttachedTo = ""
		v.AttachedEnv = ""
		v.Role = ""
		v.State = "ready"
		if err = r.saveVolume(ctx, v); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) resumeManagedCreation(ctx context.Context, vm *model.Vm, request api.CreateVmReq) (*model.Vm, error) {
	eng, err := r.engineFactory(vm.Engine, r.containerRuntime)
	if err != nil {
		return nil, err
	}
	state, err := eng.State(ctx, vm)
	if err != nil {
		return nil, err
	}
	if state == model.VmRunning || state == model.VmPaused {
		if err = r.validateManagedBindings(ctx, vm); err != nil {
			return nil, err
		}
	} else {
		vm.State = model.VmCreating
		if err = saveVM(ctx, r.db.conn, vm); err != nil {
			return nil, err
		}
		if err = r.createResources(ctx, vm, request.GuestConfig, r.imageDir+"/"+request.Image, request.Disk, request); err != nil {
			return nil, err
		}
	}
	vm.Options.CreationComplete = true
	vm.State = model.VmRunning
	if request.InitialStopped {
		vm.State = model.VmStopped
	}
	vm.Error = nil
	if err = saveVM(ctx, r.db.conn, vm); err != nil {
		return nil, err
	}
	if err = r.recount(ctx); err != nil {
		return nil, err
	}
	return vm, nil
}
