package agent

import (
	"context"
	"path/filepath"
	"time"

	"github.com/aisidev/tt/internal/storage"
	"github.com/aisidev/tt/model"
)

type BackupSetup struct {
	VM                  model.Vm
	storage             model.Storage
	root, locks, runDir string
}

func (s BackupSetup) Context(ctx context.Context) (storage.BackupContext, error) {
	vm := s.VM
	store, err := storage.New(s.storage)
	if err != nil {
		return storage.BackupContext{}, err
	}
	clone := s.root + "/clone-" + vm.ID
	directory := ""
	if vm.Engine == model.Firecracker && s.storage == model.File {
		directory = clone
	}
	var disk string
	switch {
	case s.storage == model.Zvol && vm.Engine == model.Firecracker:
		disk = clone + "/rootfs"
	case s.storage == model.Zvol:
		disk = clone
	case vm.Engine == model.Firecracker:
		disk = directory + "/rootfs.ext4"
	default:
		disk, err = store.ResolveDisk(ctx, clone)
		if err != nil {
			return storage.BackupContext{}, err
		}
	}
	dependencies := []string{}
	if vm.Options.SSH != nil {
		dependencies = append(dependencies, filepath.Join(s.runDir, "ssh-"+vm.ID+".sh"))
	}
	if directory != "" {
		dependencies = append(dependencies, filepath.Join(directory, "vmlinux"))
		if vm.Options.ConfigDiskMiB(vm.Engine) > 0 {
			dependencies = append(dependencies, filepath.Join(directory, "guest-config.ext4"))
		}
	}
	options := vm.Options
	options.RequestedDisk = 0
	data, err := model.SerdeJSON(options)
	if err != nil {
		return storage.BackupContext{}, err
	}
	backend := model.BackupReflink
	if s.storage == model.Zvol {
		backend = model.BackupZvol
	}
	return storage.BackupContext{VMID: vm.ID, Engine: vm.Engine, Backend: backend, Disk: disk, Root: s.root, Dependencies: dependencies, LockPath: filepath.Join(s.locks, "backup-"+vm.ID+".lock"), OptionsDigest: string(data), ConfigDisk: vm.Options.ConfigDiskMiB(vm.Engine) > 0, Deadline: time.Now().Add(240 * time.Second)}, nil
}
func (r *Runtime) backupSetup(vm model.Vm) BackupSetup {
	return BackupSetup{VM: vm, storage: r.storage, root: r.runtimeDir, locks: r.backupLocks, runDir: r.runDir}
}
func (r *Runtime) deleteBackups(ctx context.Context, vm *model.Vm) error {
	if !vm.Backup.HasArtifacts() {
		return nil
	}
	c, err := r.backupSetup(*vm).Context(ctx)
	if err != nil {
		return err
	}
	lock, err := c.Lock()
	if err != nil {
		return err
	}
	defer lock.Close()
	generations := []model.BackupGeneration{}
	if vm.Backup.Current != nil {
		generations = append(generations, *vm.Backup.Current)
	}
	generations = append(generations, vm.Backup.Retired...)
	for _, generation := range generations {
		if err := c.Remove(ctx, lock, generation); err != nil {
			return err
		}
	}
	if vm.Backup.Pending != nil {
		return c.Remove(ctx, lock, vm.Backup.Pending.Artifact)
	}
	return nil
}
