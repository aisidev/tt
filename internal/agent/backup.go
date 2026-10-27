package agent

import (
	"context"
	"fmt"
	"math"
	"slices"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/backup"
	"github.com/aisidev/tt/internal/capability"
	"github.com/aisidev/tt/internal/storage"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

func (r *Runtime) ConfigureBackup(ctx context.Context, enabled bool) {
	r.backupEnabled = enabled
	r.backupUnsupported = nil
	if r.storage == model.File {
		if err := storage.ProbeReflink(r.runtimeDir); err != nil {
			r.backupUnsupported = stringPointer(err.Error())
		}
	} else if !r.probe.probe(ctx, "zfs", "version") {
		r.backupUnsupported = stringPointer("backup unsupported: ZFS tools unavailable")
	}
}
func (r *Runtime) BackupView(ctx context.Context, id string) (api.BackupView, error) {
	vm, err := loadVM(ctx, r.db.conn, id)
	if err != nil {
		return api.BackupView{}, err
	}
	if vm == nil {
		return api.BackupView{}, fmt.Errorf("not found: VM %s", id)
	}
	report := r.capabilityReport()
	reason := capability.BackupUnsupported(vm.Engine, r.storage, r.backupUnsupported, &report)
	return api.BackupView{Vm: *vm, Enabled: r.backupEnabled, Supported: reason == nil, UnsupportedReason: reason}, nil
}
func (r *Runtime) BackupClaim(ctx context.Context, id string, request *model.BackupRequest) (BackupSetup, error) {
	view, err := r.BackupView(ctx, id)
	if err != nil {
		return BackupSetup{}, err
	}
	if request != nil {
		complete, err := backup.Check(view.Vm.Backup, *request)
		if err != nil {
			return BackupSetup{}, err
		}
		if complete {
			return BackupSetup{}, fmt.Errorf("completed backup operation")
		}
		if request.Action == model.BackupCreate && view.Vm.Backup.Pending == nil {
			if !r.backupEnabled {
				return BackupSetup{}, fmt.Errorf("backup admission disabled")
			}
			if view.UnsupportedReason != nil {
				return BackupSetup{}, fmt.Errorf("%s", *view.UnsupportedReason)
			}
		}
		if err := capability.Backup.RequireDesign(view.Vm.Engine, r.storage); err != nil {
			return BackupSetup{}, fmt.Errorf("backup unsupported: %w", err)
		}
		if view.Vm.State != model.VmStopped || view.Vm.PendingResources != nil {
			return BackupSetup{}, fmt.Errorf("conflict: backup requires a stopped VM without an unfinished resource update")
		}
		eng, err := r.engineFactory(view.Vm.Engine, r.containerRuntime)
		if err != nil {
			return BackupSetup{}, err
		}
		state, err := eng.State(ctx, &view.Vm)
		if err != nil {
			return BackupSetup{}, err
		}
		if state != model.VmStopped {
			return BackupSetup{}, fmt.Errorf("conflict: backup requires an actually stopped VM")
		}
	} else if view.Vm.Backup.Pending != nil {
		return BackupSetup{}, fmt.Errorf("conflict: backup operation is unfinished")
	} else if view.Vm.Backup.CleanupRetryAt > r.now() {
		return BackupSetup{}, fmt.Errorf("conflict: backup cleanup is backing off after an error")
	}
	if len(r.backupRunning) >= 2 || r.backupRunning[id] {
		return BackupSetup{}, backupBusyError{}
	}
	r.backupRunning[id] = true
	return r.backupSetup(view.Vm), nil
}

// backupBusyError rejects a claim while another backup worker holds the slot.
// Its message is the stable "conflict:" wire text; the type lets the worker's
// retry loop recognize the condition without matching the message.
type backupBusyError struct{}

func (backupBusyError) Error() string {
	return "conflict: backup worker is running; inspect or retry the same operation"
}

func (r *Runtime) BackupRelease(id string) { delete(r.backupRunning, id) }
func (r *Runtime) BackupSettled(ctx context.Context, id string, request model.BackupRequest) *uint64 {
	if r.backupRunning[id] {
		return nil
	}
	vm, err := loadVM(ctx, r.db.conn, id)
	if err != nil {
		return nil
	}
	if vm == nil {
		zero := uint64(0)
		return &zero
	}
	if vm.Backup.Pending == nil || !backup.Equal(vm.Backup.Pending.Request, request) {
		sequence := vm.Backup.Sequence
		return &sequence
	}
	return nil
}
func (r *Runtime) BackupAdmit(ctx context.Context, id string, request model.BackupRequest, witness storage.Witness) (*model.Vm, error) {
	view, err := r.BackupView(ctx, id)
	if err != nil {
		return nil, err
	}
	vm := view.Vm
	complete, err := backup.Check(vm.Backup, request)
	if err != nil {
		return nil, err
	}
	if complete {
		return nil, fmt.Errorf("completed backup operation")
	}
	if vm.Backup.Pending != nil {
		return &vm, nil
	}
	if vm.State != model.VmStopped || vm.PendingResources != nil {
		return nil, fmt.Errorf("conflict: VM changed before backup admission")
	}
	mib := bytesMiB(witness.RootBytes)
	if mib > math.MaxUint32 {
		return nil, fmt.Errorf("invalid root capacity")
	}
	rootMiB := uint32(mib)
	overhead := vm.Options.ConfigDiskMiB(vm.Engine)
	if vm.Disk < overhead || vm.Disk-overhead != rootMiB {
		return nil, fmt.Errorf("conflict: actual disk capacity differs from VM metadata")
	}
	switch request.Action {
	case model.BackupCreate:
		if len(vm.Backup.Retired) >= backup.MaxRetired {
			return nil, fmt.Errorf("conflict: backup retirement backlog is full; wait for cleanup")
		}
	case model.BackupRestore:
		for _, old := range vm.Backup.Retired {
			if old.RestoreStaging {
				return nil, fmt.Errorf("conflict: previous restore staging cleanup is unfinished")
			}
		}
		source := vm.Backup.Current
		if source == nil {
			return nil, fmt.Errorf("not found: no backup to restore")
		}
		if request.Generation == nil || *request.Generation != source.ID {
			return nil, fmt.Errorf("precondition: backup generation changed")
		}
		if source.RootBytes != witness.RootBytes || source.Dependencies != witness.Dependencies {
			return nil, fmt.Errorf("conflict: root capacity or retained boot dependencies differ from backup")
		}
	}
	if err := r.recount(ctx); err != nil {
		return nil, err
	}
	if request.Action != model.BackupDelete && r.resource.DiskFree() < rootMiB {
		return nil, fmt.Errorf("conflict: insufficient disk budget for backup staging")
	}
	backend := model.BackupReflink
	if r.storage == model.Zvol {
		backend = model.BackupZvol
	}
	vm.Backup.Pending = &model.BackupPending{Request: request, DiskIdentity: witness.DiskIdentity, Artifact: model.BackupGeneration{ID: uuid.NewString(), Backend: backend, RootBytes: witness.RootBytes, CreatedAt: r.now(), Dependencies: witness.Dependencies, RestoreStaging: request.Action == model.BackupRestore}}
	vm.Backup.Revision = uuid.NewString()
	if vm.Backup.Sequence == math.MaxUint64 {
		return nil, fmt.Errorf("conflict: backup sequence exhausted")
	}
	vm.Backup.Sequence++
	if err := saveVM(ctx, r.db.conn, &vm); err != nil {
		return nil, err
	}
	if err := r.recount(ctx); err != nil {
		return nil, err
	}
	return &vm, nil
}
func (r *Runtime) BackupFinish(ctx context.Context, id string, request model.BackupRequest, removed []string, artifact *model.BackupGeneration, resultErr error) (api.BackupView, error) {
	view, err := r.BackupView(ctx, id)
	if err != nil {
		return api.BackupView{}, err
	}
	vm := view.Vm
	pending := vm.Backup.Pending
	if pending == nil {
		return api.BackupView{}, fmt.Errorf("conflict: backup intent disappeared")
	}
	if !backup.Equal(pending.Request, request) {
		return api.BackupView{}, fmt.Errorf("conflict: backup operation changed")
	}
	if resultErr == nil {
		switch request.Action {
		case model.BackupCreate:
			if artifact == nil {
				return api.BackupView{}, fmt.Errorf("backup completed without an artifact")
			}
			if vm.Backup.Current != nil {
				vm.Backup.Retired = append(vm.Backup.Retired, *vm.Backup.Current)
			}
			vm.Backup.Current = artifact
		case model.BackupRestore:
			vm.Backup.Retired = slices.DeleteFunc(vm.Backup.Retired, func(g model.BackupGeneration) bool { return slices.Contains(removed, g.ID) })
			if artifact != nil {
				vm.Backup.Retired = append(vm.Backup.Retired, *artifact)
			}
			if vm.SSH != nil {
				vm.SSH.Initialized = false
				vm.SSH.Ready = false
				vm.SSH.CheckedAt = 0
				vm.SSH.ObservationError = stringPointer("SSH identity must be observed after disk restore")
			}
		case model.BackupDelete:
			vm.Backup.Current = nil
			vm.Backup.Retired = nil
		}
		vm.Backup.Pending = nil
		if len(vm.Backup.Retired) == 0 {
			vm.Backup.CleanupError = nil
			vm.Backup.CleanupRetryAt = 0
			vm.Backup.CleanupFailures = 0
		}
		vm.Backup.LastResult = &model.BackupReceipt{Request: request, CompletedAt: r.now()}
	} else if request.Action == model.BackupCreate {
		vm.Backup.Retired = append(vm.Backup.Retired, pending.Artifact)
		vm.Backup.Pending = nil
		vm.Backup.LastResult = &model.BackupReceipt{Request: request, CompletedAt: r.now(), Error: stringPointer(resultErr.Error())}
	} else {
		vm.Backup.Pending.Error = stringPointer(resultErr.Error())
	}
	if err := saveVM(ctx, r.db.conn, &vm); err != nil {
		return api.BackupView{}, err
	}
	if err := r.recount(ctx); err != nil {
		return api.BackupView{}, err
	}
	return r.BackupView(ctx, id)
}
func (r *Runtime) BackupCleanupFinish(ctx context.Context, id string, removed []string, cleanupError *string) error {
	view, err := r.BackupView(ctx, id)
	if err != nil {
		return err
	}
	vm := view.Vm
	if vm.Backup.Current != nil && slices.Contains(removed, vm.Backup.Current.ID) {
		return fmt.Errorf("conflict: refusing cleanup of current backup")
	}
	vm.Backup.Retired = slices.DeleteFunc(vm.Backup.Retired, func(g model.BackupGeneration) bool { return slices.Contains(removed, g.ID) })
	vm.Backup.CleanupError = cleanupError
	if cleanupError != nil {
		vm.Backup.CleanupFailures = model.SatAdd(vm.Backup.CleanupFailures, 1)
		delay := uint64(15) << min(vm.Backup.CleanupFailures, 6)
		now := r.now()
		vm.Backup.CleanupRetryAt = math.MaxUint64
		if delay <= math.MaxUint64-now {
			vm.Backup.CleanupRetryAt = now + delay
		}
	} else {
		vm.Backup.CleanupFailures = 0
		vm.Backup.CleanupRetryAt = 0
	}
	if err := saveVM(ctx, r.db.conn, &vm); err != nil {
		return err
	}
	return r.recount(ctx)
}
