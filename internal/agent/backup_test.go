package agent

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/aisidev/tt/internal/backup"
	"github.com/aisidev/tt/internal/storage"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

func backupGuest() model.Vm {
	vm := vmFixture("backup-vm")
	vm.Engine = model.Firecracker
	vm.Disk = 16
	vm.CPU = 1
	vm.Mem = 128
	return vm
}
func backupWitness() storage.Witness {
	return storage.Witness{RootBytes: 16 * 1024 * 1024, DiskIdentity: "owned-disk", Dependencies: "boot-dependencies"}
}
func backupRequest(vm model.Vm, action model.BackupAction) model.BackupRequest {
	r := model.BackupRequest{OperationID: uuid.NewString(), ExpectedRevision: vm.Backup.Revision, Action: action}
	if action == model.BackupRestore {
		r.Generation = &vm.Backup.Current.ID
	}
	return r
}
func publishBackup(t *testing.T, r *Runtime, vm model.Vm) model.Vm {
	t.Helper()
	ctx := context.Background()
	request := backupRequest(vm, model.BackupCreate)
	admitted, err := r.BackupAdmit(ctx, vm.ID, request, backupWitness())
	if err != nil {
		t.Fatal(err)
	}
	artifact := admitted.Backup.Pending.Artifact
	artifact.Identity = "owned-artifact"
	view, err := r.BackupFinish(ctx, vm.ID, request, nil, &artifact, nil)
	if err != nil {
		t.Fatal(err)
	}
	return view.Vm
}
func TestDisabledAdmissionAndQualificationClaimCannotChangeOrStartDisk(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	vm := backupGuest()
	ctx := context.Background()
	if err := saveVM(ctx, r.db.conn, &vm); err != nil {
		t.Fatal(err)
	}
	request := backupRequest(vm, model.BackupCreate)
	if _, err := r.BackupClaim(ctx, vm.ID, &request); err == nil || err.Error() != "backup admission disabled" {
		t.Fatal(err)
	}
	stored, err := loadVM(ctx, r.db.conn, vm.ID)
	if err != nil || stored.Backup.Revision != vm.Backup.Revision {
		t.Fatal(stored, err)
	}
	r.backupEnabled = true
	if _, err := r.BackupClaim(ctx, vm.ID, &request); err != nil {
		t.Fatal(err)
	}
	if r.StartVM(ctx, vm.ID) == nil || r.StopVM(ctx, vm.ID) == nil || r.DestroyVM(ctx, vm.ID) == nil {
		t.Fatal("qualification claim allowed lifecycle")
	}
	if r.BackupSettled(ctx, vm.ID, request) != nil {
		t.Fatal("active worker marked settled")
	}
	r.BackupRelease(vm.ID)
	stored, err = loadVM(ctx, r.db.conn, vm.ID)
	if err != nil || stored.State != model.VmStopped {
		t.Fatal(stored, err)
	}
}
func TestNormalizingImplicitDiskRequestDoesNotInvalidateBootDependencies(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	setup := r.backupSetup(backupGuest())
	ctx := context.Background()
	original, err := setup.Context(ctx)
	if err != nil {
		t.Fatal(err)
	}
	setup.VM.CPU = 2
	setup.VM.Mem = 256
	setup.VM.Options.RequestedDisk = 16
	updated, err := setup.Context(ctx)
	if err != nil || updated.OptionsDigest != original.OptionsDigest {
		t.Fatal(updated, err)
	}
	setup.VM.Options.DenyOutgoing = true
	changed, err := setup.Context(ctx)
	if err != nil || changed.OptionsDigest == original.OptionsDigest {
		t.Fatal(changed, err)
	}
}
func TestContainerAndJailBackupsRejectedBeforeClaimingStorage(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	r.backupEnabled = true
	ctx := context.Background()
	for _, kind := range []model.Engine{model.Docker, model.Jail} {
		vm := backupGuest()
		vm.Engine = kind
		if err := saveVM(ctx, r.db.conn, &vm); err != nil {
			t.Fatal(err)
		}
		view, err := r.BackupView(ctx, vm.ID)
		if err != nil || view.Supported || view.UnsupportedReason == nil {
			t.Fatal(view, err)
		}
		for _, action := range []model.BackupAction{model.BackupCreate, model.BackupRestore, model.BackupDelete} {
			request := backupRequest(vm, model.BackupCreate)
			request.Action = action
			if action == model.BackupRestore {
				request.Generation = stringPointer(uuid.NewString())
			}
			if _, err := r.BackupClaim(ctx, vm.ID, &request); err == nil || !strings.HasPrefix(err.Error(), "backup unsupported:") {
				t.Fatal(err)
			}
			if len(r.backupRunning) != 0 {
				t.Fatal("unsupported worker claimed")
			}
		}
	}
}
func TestBhyveZvolUsesSharedSnapshotContextAndLifecycleExclusion(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	r.backupEnabled = true
	r.storage = model.Zvol
	r.runtimeDir = "pool/runtime"
	vm := backupGuest()
	vm.Engine = model.Bhyve
	ctx := context.Background()
	if err := saveVM(ctx, r.db.conn, &vm); err != nil {
		t.Fatal(err)
	}
	request := backupRequest(vm, model.BackupCreate)
	setup, err := r.BackupClaim(ctx, vm.ID, &request)
	if err != nil {
		t.Fatal(err)
	}
	c, err := setup.Context(ctx)
	if err != nil || c.Backend != model.BackupZvol || c.Disk != "pool/runtime/clone-backup-vm" || len(c.Dependencies) != 0 {
		t.Fatal(c, err)
	}
	if r.StartVM(ctx, vm.ID) == nil || r.StopVM(ctx, vm.ID) == nil || r.DestroyVM(ctx, vm.ID) == nil {
		t.Fatal("backup exclusion lost")
	}
	r.BackupRelease(vm.ID)
}
func TestFailedRefreshPreservesCurrentAndFailedCleanupKeepsReservations(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	ctx := context.Background()
	vm := backupGuest()
	if err := saveVM(ctx, r.db.conn, &vm); err != nil {
		t.Fatal(err)
	}
	first := publishBackup(t, r, vm)
	if first.ReservedDisk() != 32 {
		t.Fatal(first.ReservedDisk())
	}
	request := backupRequest(first, model.BackupCreate)
	if _, err := r.BackupAdmit(ctx, vm.ID, request, backupWitness()); err != nil {
		t.Fatal(err)
	}
	if r.resource.DiskUsed != 48 {
		t.Fatal(r.resource)
	}
	failed, err := r.BackupFinish(ctx, vm.ID, request, nil, nil, fmt.Errorf("injected snapshot failure"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(failed.Vm.Backup.Current, first.Backup.Current) || len(failed.Vm.Backup.Retired) != 1 {
		t.Fatal(failed)
	}
	complete, err := backup.Check(failed.Vm.Backup, request)
	if err != nil || !complete {
		t.Fatal(complete, err)
	}
	if err := r.BackupCleanupFinish(ctx, vm.ID, nil, stringPointer("held by operator")); err != nil {
		t.Fatal(err)
	}
	held, err := r.BackupView(ctx, vm.ID)
	if err != nil || held.Vm.ReservedDisk() != 48 || held.Vm.Backup.CleanupRetryAt <= r.now() {
		t.Fatal(held, err)
	}
	newer := publishBackup(t, r, held.Vm)
	if newer.Backup.Current.ID == first.Backup.Current.ID || len(newer.Backup.Retired) != 2 || newer.ReservedDisk() != 64 {
		t.Fatal(newer)
	}
	refused := backupRequest(newer, model.BackupCreate)
	if _, err := r.BackupAdmit(ctx, vm.ID, refused, backupWitness()); err == nil || !strings.Contains(err.Error(), "backlog") {
		t.Fatal(err)
	}
	if _, err := r.ResizeVM(ctx, vm.ID, model.VmResources{CPU: 1, Mem: 128, Disk: 32}); err == nil {
		t.Fatal("disk resized with backup artifacts")
	}
}
func TestRestoreIntentSurvivesReopenAndCompletedRetryPreservesNewObservations(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	ctx := context.Background()
	vm := backupGuest()
	vm.SSH = &model.SshInfo{User: "root", Port: 22, HostKey: "initial-public-identity", Ready: true, Initialized: true, CheckedAt: 10}
	if err := saveVM(ctx, r.db.conn, &vm); err != nil {
		t.Fatal(err)
	}
	published := publishBackup(t, r, vm)
	request := backupRequest(published, model.BackupRestore)
	if _, err := r.BackupAdmit(ctx, vm.ID, request, backupWitness()); err != nil {
		t.Fatal(err)
	}
	if r.StartVM(ctx, vm.ID) == nil {
		t.Fatal("restore intent allowed start")
	}
	var sequence int
	var name, path string
	if err := r.db.conn.QueryRowContext(ctx, "PRAGMA database_list").Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := r.db.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	r.db, err = openAgentDatabase(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.db.Close() })
	if r.StartVM(ctx, vm.ID) == nil {
		t.Fatal("reopen lost restore intent")
	}
	pending, err := r.BackupView(ctx, vm.ID)
	if err != nil || pending.Vm.Backup.Pending == nil || !backup.Equal(pending.Vm.Backup.Pending.Request, request) {
		t.Fatal(pending, err)
	}
	restored, err := r.BackupFinish(ctx, vm.ID, request, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ssh := restored.Vm.SSH
	if ssh.Ready || ssh.Initialized || ssh.CheckedAt != 0 || ssh.HostKey != "initial-public-identity" {
		t.Fatal(ssh)
	}
	ssh.Ready = true
	ssh.Initialized = true
	ssh.CheckedAt = 20
	if err := saveVM(ctx, r.db.conn, &restored.Vm); err != nil {
		t.Fatal(err)
	}
	if _, err := r.BackupClaim(ctx, vm.ID, &request); err == nil || err.Error() != "completed backup operation" {
		t.Fatal(err)
	}
	replay, err := r.BackupView(ctx, vm.ID)
	if err != nil || replay.Vm.SSH.CheckedAt != 20 {
		t.Fatal(replay, err)
	}
}
func TestBackupSequenceExhaustionDoesNotPublishIntent(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	ctx := context.Background()
	vm := backupGuest()
	vm.Backup.Sequence = math.MaxUint64
	if err := saveVM(ctx, r.db.conn, &vm); err != nil {
		t.Fatal(err)
	}
	request := backupRequest(vm, model.BackupCreate)
	if _, err := r.BackupAdmit(ctx, vm.ID, request, backupWitness()); err == nil || err.Error() != "conflict: backup sequence exhausted" {
		t.Fatal(err)
	}
	stored, err := loadVM(ctx, r.db.conn, vm.ID)
	if err != nil || stored.Backup.Pending != nil || stored.Backup.Revision != vm.Backup.Revision {
		t.Fatal(stored, err)
	}
}
