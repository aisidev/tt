//go:build !linux && !freebsd

package storage

// Backup storage is implemented only on the supported fleet platforms. These
// declarations keep the shared packages buildable elsewhere (development
// hosts); every entry point reports unsupported.
import (
	"context"
	"errors"
	"time"

	"github.com/aisidev/tt/model"
)

var errBackupUnsupported = errors.New("disk backup is not supported on this platform")

type BackupContext struct {
	VMID                 string
	Engine               model.Engine
	Backend              model.BackupBackend
	Disk, Root, LockPath string
	Dependencies         []string
	// OptionsDigest is serialized boot options, not a precomputed hash.
	OptionsDigest string
	ConfigDisk    bool
	Deadline      time.Time
}
type Witness struct {
	RootBytes                  uint64
	DiskIdentity, Dependencies string
}
type WorkerLock struct{}

func (*WorkerLock) Close() error { return nil }
func (c BackupContext) Lock() (*WorkerLock, error) {
	return nil, errBackupUnsupported
}
func (c BackupContext) Inspect(ctx context.Context, lock *WorkerLock, probe bool) (Witness, error) {
	return Witness{}, errBackupUnsupported
}
func (c BackupContext) Verify(ctx context.Context, lock *WorkerLock, g model.BackupGeneration) (model.BackupGeneration, error) {
	return model.BackupGeneration{}, errBackupUnsupported
}
func (c BackupContext) Create(ctx context.Context, lock *WorkerLock, p model.BackupPending) (model.BackupGeneration, error) {
	return model.BackupGeneration{}, errBackupUnsupported
}
func (c BackupContext) Restore(ctx context.Context, lock *WorkerLock, p model.BackupPending, source model.BackupGeneration) (*model.BackupGeneration, error) {
	return nil, errBackupUnsupported
}
func (c BackupContext) PrepareRestore(ctx context.Context, lock *WorkerLock, source model.BackupGeneration, retired []model.BackupGeneration) ([]string, error) {
	return nil, errBackupUnsupported
}
func (c BackupContext) Remove(ctx context.Context, lock *WorkerLock, g model.BackupGeneration) error {
	return errBackupUnsupported
}

func ProbeReflink(root string) error { return errBackupUnsupported }
