package agent

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/capability"
	"github.com/aisidev/tt/internal/engine"
	"github.com/aisidev/tt/internal/guestconfig"
	"github.com/aisidev/tt/internal/sshboot"
	"github.com/aisidev/tt/model"
	"golang.org/x/sys/unix"
)

func (r *Runtime) requireVM(ctx context.Context, id string) (*model.Vm, error) {
	vm, err := loadVM(ctx, r.db.conn, id)
	if err != nil {
		return nil, err
	}
	if vm == nil {
		return nil, fmt.Errorf("VM not found: %s", id)
	}
	return vm, nil
}
func (r *Runtime) StopVM(ctx context.Context, id string, options ...model.StopOptions) error {
	if len(options) > 0 && !model.ValidStopOptions(options[0]) {
		return &api.BoundaryError{Class: "terminal", Code: "invalid_stop_policy", Message: "invalid stop policy"}
	}
	vm, err := r.requireVM(ctx, id)
	if err != nil {
		return err
	}
	if err := r.backupGuard(vm); err != nil {
		return err
	}
	if vm.State == model.VmStopped && (vm.Error == nil || !strings.HasPrefix(*vm.Error, "stop unfinished:")) {
		return nil
	}
	if vm.State == model.VmCreating || vm.State == model.VmDeleting {
		return fmt.Errorf("VM is %s", vm.State)
	}
	eng, err := r.engineFactory(vm.Engine, r.containerRuntime)
	if err != nil {
		return err
	}
	if len(options) > 0 {
		eng, err = engine.ApplyStopPolicy(eng, options[0])
		if err != nil {
			return err
		}
	}
	if err := eng.Stop(ctx, vm); err != nil {
		vm.Error = stringPointer("stop unfinished: " + err.Error())
		if saveErr := saveVM(ctx, r.db.conn, vm); saveErr != nil {
			return saveErr
		}
		return err
	}
	vm.State = model.VmStopped
	vm.Error = nil
	if vm.SSH != nil {
		vm.SSH.Ready = false
		vm.SSH.CheckedAt = r.now()
	}
	if err := saveVM(ctx, r.db.conn, vm); err != nil {
		return err
	}
	return r.recount(ctx)
}
func (r *Runtime) StartVM(ctx context.Context, id string) error {
	return r.startVM(ctx, id, nil)
}
func (r *Runtime) startVM(ctx context.Context, id string, files model.GuestConfig) error {
	vm, err := r.requireVM(ctx, id)
	if err != nil {
		return err
	}
	if err := r.backupGuard(vm); err != nil {
		return err
	}
	if vm.PendingResources != nil {
		return fmt.Errorf("resource update unfinished; retry the recorded resources before starting")
	}
	if files != nil {
		if capability.Require(r.capabilityReport(), model.Firecracker, r.storage, capability.GuestConfig) != nil {
			return diskError("terminal", "incompatible_protocol", "host does not support guest configuration")
		}
		if len(files) == 0 {
			return diskError("terminal", "invalid_request", "guest configuration cannot be empty")
		}
		if err := guestconfig.Validate(vm.Engine, files, vm.Options.IsolatedNetwork); err != nil {
			return diskError("terminal", "invalid_request", "invalid guest configuration")
		}
		if vm.Engine != model.Firecracker || vm.Options.GuestConfigDigest == nil && vm.Options.SSH == nil {
			return diskError("terminal", "incompatible_protocol", "reseed requires an established Firecracker configuration drive")
		}
	}
	if vm.State == model.VmRunning {
		if files != nil && (vm.Options.GuestConfigDigest == nil || *vm.Options.GuestConfigDigest != *guestconfig.Digest(files)) {
			return diskError("state", "state_conflict", "running guest configuration cannot be replaced")
		}
		return nil
	}
	if vm.State != model.VmStopped && vm.State != model.VmPaused {
		return fmt.Errorf("cannot start VM in state %s", vm.State)
	}
	if err := r.validateManagedBindings(ctx, vm); err != nil {
		return err
	}
	if err := r.recount(ctx); err != nil {
		return err
	}
	if vm.State == model.VmStopped && !r.resource.CanFit(vm.CPU, vm.Engine.MemoryReservation(vm.Mem), 0) {
		return fmt.Errorf("insufficient resources to restart VM")
	}
	if files != nil {
		// INV-03/04/14: the command journal already owns this exact input. Prove
		// physical stop before replacing any bootstrap bytes; paused is insufficient.
		if vm.State != model.VmStopped {
			return diskError("state", "state_conflict", "guest configuration requires physical stop")
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
			return diskError("state", "state_conflict", "guest configuration requires physical stop")
		}
		config := model.GuestConfig{}
		for name, value := range files {
			config[name] = value
		}
		if _, present := config[sshboot.SeedFile]; present {
			return diskError("denied", "capability_denied", "managed SSH seed is reserved")
		}
		if vm.Options.SSH != nil {
			data, err := readOriginalSSHSeed(filepath.Join(r.runDir, "ssh-"+vm.ID+".sh"))
			if err != nil {
				return diskError("unknown", "recovery_evidence_unavailable", "original SSH seed is unavailable")
			}
			config[sshboot.SeedFile] = string(data)
		}
		directory, err := r.store.FirecrackerDir(ctx, r.clonePath(vm))
		if err != nil {
			return err
		}
		if err = guestconfig.WriteDisk(ctx, directory, config); err != nil {
			return diskError("unknown", "persistence_unavailable", "guest configuration publication is uncertain")
		}
		vm.Options.GuestConfigDigest = guestconfig.Digest(files)
		if err = saveVM(ctx, r.db.conn, vm); err != nil {
			return err
		}
	}
	previous := vm.State
	vm.State = model.VmCreating
	if err := saveVM(ctx, r.db.conn, vm); err != nil {
		return err
	}
	launch := func() error {
		if vm.Engine != model.Docker {
			if err := r.ensureNetwork(ctx); err != nil {
				return err
			}
		}
		if err := r.restoreNetwork(ctx, vm); err != nil {
			return err
		}
		eng, err := r.engineFactory(vm.Engine, r.containerRuntime)
		if err != nil {
			return err
		}
		if previous == model.VmStopped && (vm.Engine == model.Qemu || vm.Engine == model.Firecracker || vm.Engine == model.Bhyve || vm.Engine == model.Jail) {
			path, err := r.imagePath(ctx, vm)
			if err != nil {
				return err
			}
			return eng.Create(ctx, vm, path, r.store.DiskFormat(), vm.Options.SSHKeys)
		}
		return eng.Start(ctx, vm)
	}
	if err := launch(); err != nil {
		vm.State = model.VmFailed
		vm.Error = stringPointer(err.Error())
		if saveErr := saveVM(ctx, r.db.conn, vm); saveErr != nil {
			return saveErr
		}
		if recountErr := r.recount(ctx); recountErr != nil {
			return recountErr
		}
		return err
	}
	vm.State = model.VmRunning
	vm.Error = nil
	if err := saveVM(ctx, r.db.conn, vm); err != nil {
		return err
	}
	return r.recount(ctx)
}
func (r *Runtime) ResizeVM(ctx context.Context, id string, target model.VmResources) (*model.Vm, error) {
	if err := r.checkpointGuard(ctx, id); err != nil {
		return nil, err
	}
	vm, err := r.requireVM(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := r.backupGuard(vm); err != nil {
		return nil, err
	}
	if vm.Backup.HasArtifacts() && target.Disk != model.SatSub(vm.Disk, vm.Options.ConfigDiskMiB(vm.Engine)) {
		return nil, fmt.Errorf("conflict: remove backups and finish cleanup before disk resizing")
	}
	if err := capability.Resources.RequireDesign(vm.Engine, r.storage); err != nil {
		return nil, err
	}
	if vm.PendingResources == nil {
		if err := capability.Require(r.capabilityReport(), vm.Engine, r.storage, capability.Resources); err != nil {
			return nil, err
		}
	}
	if target.CPU == 0 || target.Mem == 0 || target.Disk == 0 {
		return nil, fmt.Errorf("cpu, memory and root disk must be > 0")
	}
	if vm.State != model.VmStopped {
		return nil, fmt.Errorf("resource updates require a confirmed stopped VM")
	}
	eng, err := r.engineFactory(vm.Engine, r.containerRuntime)
	if err != nil {
		return nil, err
	}
	state, err := eng.State(ctx, vm)
	if err != nil {
		return nil, err
	}
	if state != model.VmStopped {
		return nil, fmt.Errorf("resource updates require a confirmed stopped VM")
	}
	if vm.PendingResources != nil && *vm.PendingResources != target {
		return nil, fmt.Errorf("resource update unfinished; retry the recorded target first")
	}
	path := r.clonePath(vm)
	var bytes uint64
	if vm.Engine == model.Qemu {
		bytes, err = r.store.QemuSize(ctx, path)
	} else {
		bytes, err = r.store.FirecrackerSize(ctx, path)
	}
	if err != nil {
		return nil, err
	}
	current := bytesMiB(bytes)
	overhead := vm.Options.ConfigDiskMiB(vm.Engine)
	if uint64(target.Disk) < current || target.Disk < model.SatSub(vm.Disk, overhead) || target.Disk < vm.Options.RequestedDisk {
		return nil, fmt.Errorf("disk shrinking is not supported")
	}
	if target.Disk > math.MaxUint32-overhead {
		return nil, fmt.Errorf("disk capacity overflow")
	}
	disk := target.Disk + overhead
	if err := r.recount(ctx); err != nil {
		return nil, err
	}
	if !r.resource.CanFit(target.CPU, vm.Engine.MemoryReservation(target.Mem), model.SatSub(disk, vm.ReservedDisk())) {
		return nil, fmt.Errorf("insufficient resources for resource update")
	}
	if vm.PendingResources == nil && vm.CPU == target.CPU && vm.Mem == target.Mem && vm.Options.RequestedDisk == target.Disk && vm.Disk == disk {
		return vm, nil
	}
	// A Firecracker resize grows the backing disk before the ext4 filesystem.
	// Pending retries must finish the filesystem even when the disk is at target.
	// QEMU has no separate filesystem step and can skip same-size disk growth.
	grow := uint64(target.Disk) > current || (vm.Engine == model.Firecracker && vm.PendingResources != nil)
	vm.PendingResources = &target
	vm.Error = stringPointer("resource update unfinished; retry the recorded target")
	if err := saveVM(ctx, r.db.conn, vm); err != nil {
		return nil, err
	}
	if err := r.recount(ctx); err != nil {
		return nil, err
	}
	if grow {
		if vm.Engine == model.Qemu {
			err = r.store.ResizeDisk(ctx, path, target.Disk)
		} else {
			err = r.store.ResizeFirecracker(ctx, path, target.Disk)
		}
	}
	if err != nil {
		vm.Error = stringPointer("resource update unfinished: " + err.Error())
		if saveErr := saveVM(ctx, r.db.conn, vm); saveErr != nil {
			return nil, saveErr
		}
		return nil, err
	}
	vm.CPU = target.CPU
	vm.Mem = target.Mem
	vm.Disk = disk
	vm.Options.RequestedDisk = target.Disk
	vm.PendingResources = nil
	vm.Error = nil
	if err := saveVM(ctx, r.db.conn, vm); err != nil {
		return nil, err
	}
	if err := r.recount(ctx); err != nil {
		return nil, err
	}
	return vm, nil
}
func (r *Runtime) DestroyVM(ctx context.Context, id string) error {
	if err := r.checkpointGuard(ctx, id); err != nil {
		return err
	}
	vm, err := loadVM(ctx, r.db.conn, id)
	if err != nil || vm == nil {
		return err
	}
	if r.backupRunning[id] {
		return fmt.Errorf("conflict: backup writer is active; retry deletion after it settles")
	}
	vm.State = model.VmDeleting
	if err := saveVM(ctx, r.db.conn, vm); err != nil {
		return err
	}
	eng, err := r.engineFactory(vm.Engine, r.containerRuntime)
	if err != nil {
		return err
	}
	if err := eng.Stop(ctx, vm); err != nil {
		vm.Error = stringPointer(err.Error())
		if saveErr := saveVM(ctx, r.db.conn, vm); saveErr != nil {
			return saveErr
		}
		return err
	}
	if err := r.deleteBackups(ctx, vm); err != nil {
		return err
	}
	vm.Backup.Current = nil
	vm.Backup.Pending = nil
	vm.Backup.Retired = nil
	if err := saveVM(ctx, r.db.conn, vm); err != nil {
		return err
	}
	failures := []string{}
	collect := func(err error) {
		if err != nil {
			failures = append(failures, err.Error())
		}
	}
	collect(eng.Destroy(ctx, vm))
	if r.sshIngress != nil {
		collect(r.sshIngress.Remove(ctx, id))
	}
	if vm.Engine != model.Docker {
		collect(r.network.RemovePortForwards(ctx, vm.IP))
		collect(r.network.AllowOutgoing(ctx, vm.IP))
		if vm.Engine != model.Jail {
			collect(r.network.DestroyTap(ctx, id))
		}
		if runtime.GOOS == "linux" && vm.Options.IsolatedNetwork {
			collect(r.network.RemoveIsolation(ctx, id))
		}
		if !vm.Options.RootManaged {
			collect(r.store.RemoveImage(ctx, r.clonePath(vm)))
		}
		collect(r.detachDeletedVM(ctx, vm))
	}
	if len(failures) > 0 {
		message := strings.Join(failures, "; ")
		vm.Error = &message
		if err := saveVM(ctx, r.db.conn, vm); err != nil {
			return err
		}
		return fmt.Errorf("%s", message)
	}
	if err := unix.Unlink(filepath.Join(r.runDir, "ssh-"+id+".sh")); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := deleteVM(ctx, r.db.conn, id); err != nil {
		return err
	}
	return r.recount(ctx)
}
func (r *Runtime) ReconcileVM(ctx context.Context, id string) error {
	vm, err := loadVM(ctx, r.db.conn, id)
	if err != nil || vm == nil {
		return err
	}
	if vm.State == model.VmDeleting {
		return r.DestroyVM(ctx, id)
	}
	needsRecovery := vm.Error != nil
	wasRunning := vm.State == model.VmRunning || vm.State == model.VmPaused
	eng, err := r.engineFactory(vm.Engine, r.containerRuntime)
	var actual model.VmState
	if err == nil {
		actual, err = eng.State(ctx, vm)
	}
	if err != nil {
		vm.Error = stringPointer("cannot query engine: " + err.Error())
	} else {
		if vm.Error != nil && strings.HasPrefix(*vm.Error, "cannot query engine:") {
			vm.Error = nil
		}
		live := actual == model.VmRunning || actual == model.VmPaused
		if vm.State == model.VmCreating && !live {
			vm.State = model.VmFailed
			vm.Error = stringPointer("operation interrupted; delete this VM to clean up and recreate")
		} else if vm.State != model.VmFailed || live {
			vm.State = actual
		}
		if actual == model.VmStopped && vm.PendingResources == nil && vm.Error != nil && (*vm.Error == "network recovery pending" || strings.HasPrefix(*vm.Error, "network recovery failed:")) {
			vm.Error = nil
		}
		if live && (!wasRunning || needsRecovery || !r.networkReady) {
			var recovery error
			if vm.Engine != model.Docker {
				recovery = r.ensureNetwork(ctx)
				if recovery == nil {
					recovery = r.restoreNetwork(ctx, vm)
				}
			}
			if recovery == nil {
				vm.Error = nil
			} else {
				r.networkReady = false
				vm.Error = stringPointer("network recovery failed: " + recovery.Error())
			}
		}
	}
	if vm.SSH != nil {
		ssh := vm.SSH
		ssh.Host = ""
		if r.sshIngress != nil {
			ssh.Host = r.sshIngress.PublicAddress.String()
		}
		reachable := vm.State == model.VmRunning && sshboot.Ready(ctx, vm.IP)
		if reachable && !ssh.Initialized {
			ssh.Initialized = sshboot.InitialIdentityReady(ctx, vm.IP, ssh.HostKey)
		}
		ssh.Ready = reachable && ssh.Initialized
		if ssh.ObservationError != nil && vm.State == model.VmRunning {
			if ssh.Ready {
				ssh.ObservationError = nil
			} else if reachable {
				ssh.ObservationError = stringPointer("initial SSH identity not confirmed after disk restore")
			} else {
				ssh.ObservationError = stringPointer("SSH banner unavailable after disk restore")
			}
		}
		ssh.CheckedAt = r.now()
	}
	return saveVM(ctx, r.db.conn, vm)
}
