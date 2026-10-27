package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/capability"
	"github.com/aisidev/tt/internal/engine"
	"github.com/aisidev/tt/internal/storage"
	"github.com/aisidev/tt/model"
)

type fakeEngine struct {
	state                            model.VmState
	creates, starts, stops, destroys int
	createErr, stopErr, destroyErr   error
	beforeCreate                     func(*model.Vm)
	lastPath                         string
}

func (e *fakeEngine) Create(_ context.Context, vm *model.Vm, path, format string, keys []string) error {
	e.creates++
	e.lastPath = path
	if e.beforeCreate != nil {
		e.beforeCreate(vm)
	}
	if e.createErr != nil {
		return e.createErr
	}
	e.state = model.VmRunning
	return nil
}
func (e *fakeEngine) Start(context.Context, *model.Vm) error {
	e.starts++
	e.state = model.VmRunning
	return nil
}
func (e *fakeEngine) Stop(context.Context, *model.Vm) error {
	e.stops++
	if e.stopErr != nil {
		return e.stopErr
	}
	e.state = model.VmStopped
	return nil
}
func (e *fakeEngine) Destroy(context.Context, *model.Vm) error                { e.destroys++; return e.destroyErr }
func (e *fakeEngine) State(context.Context, *model.Vm) (model.VmState, error) { return e.state, nil }
func (*fakeEngine) Name() string                                              { return "fake" }

type fakeStore struct {
	storage.ImageStore
	size                     uint64
	filesystemSize           uint64
	clones, removes, resizes int
	resizeErr, errorSize     error
	beforeClone              func()
	lastClone                string
}

func (s *fakeStore) CloneImage(_ context.Context, source, dest string) error {
	s.clones++
	s.lastClone = dest
	if s.beforeClone != nil {
		s.beforeClone()
	}
	return nil
}
func (s *fakeStore) RemoveImage(context.Context, string) error         { s.removes++; return nil }
func (s *fakeStore) ImageExists(context.Context, string) (bool, error) { return true, nil }
func (s *fakeStore) ResolveDisk(_ context.Context, path string) (string, error) {
	return path + "/disk", nil
}
func (s *fakeStore) DiskFormat() string                                { return "qcow2" }
func (s *fakeStore) BhyveSize(context.Context, string) (uint64, error) { return s.size, s.errorSize }
func (s *fakeStore) QemuSize(context.Context, string) (uint64, error)  { return s.size, s.errorSize }
func (s *fakeStore) FirecrackerSize(context.Context, string) (uint64, error) {
	return s.size, s.errorSize
}
func (s *fakeStore) FirecrackerDir(_ context.Context, path string) (string, error) { return path, nil }
func (s *fakeStore) ResizeDisk(_ context.Context, _ string, size uint32) error {
	s.resizes++
	s.size = uint64(size) * 1024 * 1024
	return s.resizeErr
}
func (s *fakeStore) ResizeFirecracker(ctx context.Context, path string, size uint32) error {
	if err := s.ResizeDisk(ctx, path, size); err != nil {
		return err
	}
	s.filesystemSize = s.size
	return nil
}

type fakeNetwork struct {
	calls []string
	tap   bool
	err   error
}

func (n *fakeNetwork) call(name string) error                        { n.calls = append(n.calls, name); return n.err }
func (n *fakeNetwork) SetupBridge(context.Context) error             { return n.call("bridge") }
func (n *fakeNetwork) SetupNAT(context.Context) error                { return n.call("nat") }
func (n *fakeNetwork) Isolate(context.Context, string, string) error { return n.call("isolate") }
func (n *fakeNetwork) RemoveIsolation(context.Context, string) error {
	return n.call("remove-isolation")
}
func (n *fakeNetwork) TapExists(context.Context, string) (bool, error) { return n.tap, n.err }
func (n *fakeNetwork) CreateTap(context.Context, string, string) error { return n.call("tap") }
func (n *fakeNetwork) DestroyTap(context.Context, string) error        { return n.call("remove-tap") }
func (n *fakeNetwork) RemovePortForwards(context.Context, string) error {
	return n.call("remove-ports")
}
func (n *fakeNetwork) AddPortForward(context.Context, uint16, string, uint16) error {
	return n.call("port")
}
func (n *fakeNetwork) DenyOutgoing(context.Context, string) error  { return n.call("deny") }
func (n *fakeNetwork) AllowOutgoing(context.Context, string) error { return n.call("allow") }
func runtimeFixture(t *testing.T) (*Runtime, *fakeEngine, *fakeStore) {
	t.Helper()
	d := testDatabase(t)
	eng := &fakeEngine{state: model.VmStopped}
	store := &fakeStore{size: 128 * 1024 * 1024}
	root := t.TempDir()
	r := &Runtime{hostID: "host", db: d, engines: []model.Engine{model.Qemu, model.Firecracker, model.Docker, model.Bhyve, model.Jail}, probedCapabilities: capability.LegacyAll, store: store, storage: model.File, imageDir: root + "/images", runtimeDir: root + "/runtime", runDir: root, resource: model.Resource{CPUTotal: 8, MemTotal: 4096, DiskTotal: 16384}, network: &fakeNetwork{tap: true}, portStart: 20000, portEnd: 20010, portAvailable: func(uint16) bool { return true }, engineFactory: func(model.Engine, *engine.ContainerRuntime) (engine.Engine, error) { return eng, nil }, backupRunning: map[string]bool{}, backupLocks: root, now: func() uint64 { return 1000 }}
	return r, eng, store
}
func requestFixture() api.CreateVmReq {
	return api.CreateVmReq{VmID: "vm1", EnvID: "env", Image: "alpine", Engine: model.Qemu, CPU: 2, Mem: 256, Disk: 128, Ports: model.List[uint16]{80, 22, 80}}
}
func TestRepeatedCreateIsIdempotentAndChangedRequestRejected(t *testing.T) {
	r, e, s := runtimeFixture(t)
	ctx := context.Background()
	req := requestFixture()
	s.beforeClone = func() {
		vm, err := loadVM(ctx, r.db.conn, req.VmID)
		if err != nil || vm == nil || vm.State != model.VmCreating || vm.PortMap[80] != 20000 || vm.PortMap[22] != 20001 {
			t.Fatal(vm, err)
		}
	}
	first, err := r.CreateVM(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.CreateVM(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.Backup.Revision != second.Backup.Revision || e.creates != 1 || s.clones != 1 {
		t.Fatal(first, second, e.creates, s.clones)
	}
	req.Mem++
	if _, err := r.CreateVM(ctx, req); err == nil {
		t.Fatal("changed retry accepted")
	}
}
func TestExactCreationRetryIsNotFreshAdmissionAfterCapabilityWithdrawal(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	ctx := context.Background()
	req := requestFixture()
	if _, err := r.CreateVM(ctx, req); err != nil {
		t.Fatal(err)
	}
	r.engines = nil
	r.probedCapabilities = nil
	if _, err := r.CreateVM(ctx, req); err != nil {
		t.Fatal(err)
	}
	req.VmID = "new"
	if _, err := r.CreateVM(ctx, req); err == nil {
		t.Fatal("new admission bypassed capabilities")
	}
	vm, err := loadVM(ctx, r.db.conn, "new")
	if err != nil || vm != nil {
		t.Fatal(vm, err)
	}
}
func TestFailedCreateRetainsIntentAndResources(t *testing.T) {
	r, e, s := runtimeFixture(t)
	ctx := context.Background()
	e.createErr = fmt.Errorf("readiness timeout")
	if _, err := r.CreateVM(ctx, requestFixture()); err == nil {
		t.Fatal("timeout accepted")
	}
	vm, err := loadVM(ctx, r.db.conn, "vm1")
	if err != nil || vm == nil || vm.State != model.VmFailed || vm.Error == nil || s.removes != 0 {
		t.Fatal(vm, err)
	}
	if err := r.recount(ctx); err != nil {
		t.Fatal(err)
	}
	if r.resource.CPUUsed != 2 || r.resource.DiskUsed != 128 {
		t.Fatal(r.resource)
	}
	if _, err := r.CreateVM(ctx, requestFixture()); err == nil {
		t.Fatal("failed identity reused")
	}
}
func TestStopStartUsesRetainedDiskForColdBootAndResumeForPause(t *testing.T) {
	r, e, s := runtimeFixture(t)
	ctx := context.Background()
	if _, err := r.CreateVM(ctx, requestFixture()); err != nil {
		t.Fatal(err)
	}
	if err := r.StopVM(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}
	if r.resource.CPUUsed != 0 || r.resource.MemUsed != 0 || r.resource.DiskUsed != 128 {
		t.Fatal(r.resource)
	}
	if err := r.StartVM(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}
	if e.creates != 2 || e.starts != 0 || s.clones != 1 || s.removes != 0 {
		t.Fatal(e, s)
	}
	vm, err := loadVM(ctx, r.db.conn, "vm1")
	if err != nil {
		t.Fatal(err)
	}
	vm.State = model.VmPaused
	e.state = model.VmPaused
	if err := saveVM(ctx, r.db.conn, vm); err != nil {
		t.Fatal(err)
	}
	if err := r.StartVM(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}
	if e.creates != 2 || e.starts != 1 {
		t.Fatal(e)
	}
}
func TestRetryOfInterruptedStopConfirmsExit(t *testing.T) {
	r, e, _ := runtimeFixture(t)
	ctx := context.Background()
	vm := vmFixture("vm1")
	vm.Error = stringPointer("stop unfinished: previous attempt")
	if err := saveVM(ctx, r.db.conn, &vm); err != nil {
		t.Fatal(err)
	}
	e.stopErr = fmt.Errorf("still alive")
	if err := r.StopVM(ctx, vm.ID); err == nil {
		t.Fatal("stop failure hidden")
	}
	e.stopErr = nil
	if err := r.StopVM(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	got, err := loadVM(ctx, r.db.conn, vm.ID)
	if err != nil || got.Error != nil || e.stops != 2 {
		t.Fatal(got, err, e.stops)
	}
	if err := r.StopVM(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	if e.stops != 2 {
		t.Fatal("confirmed stop repeated")
	}
}
func TestFailedDeleteKeepsRecordAndReservationsThenRetrySucceeds(t *testing.T) {
	r, e, s := runtimeFixture(t)
	ctx := context.Background()
	if _, err := r.CreateVM(ctx, requestFixture()); err != nil {
		t.Fatal(err)
	}
	e.stopErr = fmt.Errorf("cannot confirm stopped")
	if err := r.DestroyVM(ctx, "vm1"); err == nil {
		t.Fatal("stop failure hidden")
	}
	vm, err := loadVM(ctx, r.db.conn, "vm1")
	if err != nil || vm.State != model.VmDeleting || s.removes != 0 {
		t.Fatal(vm, err, s.removes)
	}
	if err := r.recount(ctx); err != nil {
		t.Fatal(err)
	}
	if r.resource.CPUUsed != 2 || r.resource.DiskUsed != 128 {
		t.Fatal(r.resource)
	}
	e.stopErr = nil
	if err := r.DestroyVM(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}
	vm, err = loadVM(ctx, r.db.conn, "vm1")
	if err != nil || vm != nil || s.removes != 1 || r.resource.DiskUsed != 0 {
		t.Fatal(vm, err, s.removes, r.resource)
	}
}
func TestResourceUpdateRetriesAfterReopenAndKeepsDiskAndIdentity(t *testing.T) {
	r, e, s := runtimeFixture(t)
	ctx := context.Background()
	created, err := r.CreateVM(ctx, requestFixture())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.StopVM(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	target := model.VmResources{CPU: 1, Mem: 128, Disk: 256}
	s.resizeErr = fmt.Errorf("growth outcome unknown")
	if _, err := r.ResizeVM(ctx, created.ID, target); err == nil {
		t.Fatal("resize failure hidden")
	}
	vm, err := loadVM(ctx, r.db.conn, created.ID)
	if err != nil || vm.PendingResources == nil || vm.Disk != 128 || vm.ReservedDisk() != 256 {
		t.Fatal(vm, err)
	}
	if err := r.StartVM(ctx, created.ID); err == nil {
		t.Fatal("unfinished resize started")
	}
	changed := target
	changed.Disk++
	if _, err := r.ResizeVM(ctx, created.ID, changed); err == nil {
		t.Fatal("recorded target replaced")
	}
	// Reopen the SQLite connection, retaining only the fake host's actual disk.
	var sequence int
	var name, path string
	if err := r.db.conn.QueryRowContext(ctx, "PRAGMA database_list").Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := r.db.Close(); err != nil {
		t.Fatal(err)
	}
	r.db, err = openAgentDatabase(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.db.Close() })
	r.probedCapabilities = nil
	s.resizeErr = nil
	updated, err := r.ResizeVM(ctx, created.ID, target)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != created.ID || updated.IP != created.IP || updated.PendingResources != nil || updated.Disk != 256 || updated.CPU != 1 || updated.Mem != 128 || s.clones != 1 || e.creates != 1 {
		t.Fatal(updated, s, e)
	}
}
func TestCorruptRecordDoesNotBlockHealthyCleanupOrReleaseUnknownCapacity(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	ctx := context.Background()
	if _, err := r.CreateVM(ctx, requestFixture()); err != nil {
		t.Fatal(err)
	}
	execute(t, r.db.conn, "INSERT INTO vms VALUES ('broken','not-json')")
	if err := r.StopVM(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}
	if r.resource.CPUUsed != r.resource.CPUTotal || r.resource.DiskUsed != r.resource.DiskTotal || r.resource.VmCount != model.MaxVMs {
		t.Fatal(r.resource)
	}
	if err := r.DestroyVM(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}
	var data sqlText
	if err := r.db.conn.QueryRowContext(ctx, "SELECT data FROM vms WHERE id='broken'").Scan(&data); err != nil || data != "not-json" {
		t.Fatal(data, err)
	}
}
func TestConfirmedStopClearsOnlyObsoleteNetworkRecoveryErrors(t *testing.T) {
	for _, message := range []string{"network recovery pending", "network recovery failed: lost rules", "resource update unfinished: retain"} {
		r, _, _ := runtimeFixture(t)
		ctx := context.Background()
		vm := vmFixture("vm1")
		vm.State = model.VmRunning
		vm.Error = &message
		if err := saveVM(ctx, r.db.conn, &vm); err != nil {
			t.Fatal(err)
		}
		if err := r.ReconcileVM(ctx, vm.ID); err != nil {
			t.Fatal(err)
		}
		got, err := loadVM(ctx, r.db.conn, vm.ID)
		if err != nil || got.State != model.VmStopped {
			t.Fatal(got, err)
		}
		if message == "resource update unfinished: retain" {
			if got.Error == nil || *got.Error != message {
				t.Fatal(got)
			}
		} else if got.Error != nil {
			t.Fatal(got)
		}
	}
}
func TestJailRejectsZvolBeforeCreatingRecordOrResources(t *testing.T) {
	r, e, s := runtimeFixture(t)
	r.storage = model.Zvol
	req := requestFixture()
	req.Engine = model.Jail
	req.Disk = 0
	if _, err := r.CreateVM(context.Background(), req); err == nil {
		t.Fatal("jail zvol accepted")
	}
	vm, err := loadVM(context.Background(), r.db.conn, req.VmID)
	if err != nil || vm != nil || e.creates != 0 || s.clones != 0 {
		t.Fatal(vm, err)
	}
}
func TestBhyveCapacityIsCheckedBeforeAllocatingResources(t *testing.T) {
	r, e, s := runtimeFixture(t)
	s.size = 32768 * 1024 * 1024
	req := requestFixture()
	req.Engine = model.Bhyve
	req.Disk = 0
	if _, err := r.CreateVM(context.Background(), req); err == nil {
		t.Fatal("bhyve oversized disk admitted")
	}
	vm, err := loadVM(context.Background(), r.db.conn, req.VmID)
	if err != nil || vm != nil || s.clones != 0 || e.creates != 0 {
		t.Fatal(vm, err)
	}
}
func TestJailReceivesRetainedRootDirectory(t *testing.T) {
	r, e, _ := runtimeFixture(t)
	req := requestFixture()
	req.Engine = model.Jail
	req.Disk = 0
	vm, err := r.CreateVM(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if e.lastPath != filepath.Join(r.runtimeDir, "clone-"+vm.ID) {
		t.Fatal(e.lastPath)
	}
}

func TestLateReadinessClearsErrorsWithoutRestart(t *testing.T) {
	r, e, _ := runtimeFixture(t)
	ctx := context.Background()
	vm := vmFixture("late")
	vm.State = model.VmFailed
	vm.Error = stringPointer("startup readiness timed out")
	e.state = model.VmRunning
	if err := saveVM(ctx, r.db.conn, &vm); err != nil {
		t.Fatal(err)
	}
	if err := r.ReconcileVM(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := loadVM(ctx, r.db.conn, vm.ID)
	if err != nil || recovered.State != model.VmRunning || recovered.Error != nil || e.creates != 0 || e.starts != 0 {
		t.Fatal(recovered, err, e)
	}
}
func TestDirectResizeRejectsMissingCapabilityWithoutSavingIntent(t *testing.T) {
	r, _, s := runtimeFixture(t)
	ctx := context.Background()
	vm := vmFixture("vm1")
	vm.Disk = 128
	vm.Options.RequestedDisk = 128
	if err := saveVM(ctx, r.db.conn, &vm); err != nil {
		t.Fatal(err)
	}
	r.probedCapabilities = nil
	if _, err := r.ResizeVM(ctx, vm.ID, model.VmResources{CPU: 1, Mem: 128, Disk: 256}); err == nil {
		t.Fatal("unqualified resize admitted")
	}
	retained, err := loadVM(ctx, r.db.conn, vm.ID)
	if err != nil || retained.PendingResources != nil || retained.Disk != 128 || s.resizes != 0 {
		t.Fatal(retained, err, s.resizes)
	}
}
func TestFirecrackerReceivesRootDirectoryWithoutLocalInfrastructure(t *testing.T) {
	r, e, _ := runtimeFixture(t)
	ctx := context.Background()
	req := requestFixture()
	req.Engine = model.Firecracker
	req.Disk = 0
	vm, err := r.CreateVM(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if e.lastPath != r.clonePath(vm) {
		t.Fatal(e.lastPath)
	}
}

func TestResizeRetrySkipsDiskWorkAlreadyAtTarget(t *testing.T) {
	r, _, s := runtimeFixture(t)
	ctx := context.Background()
	created, err := r.CreateVM(ctx, requestFixture())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.StopVM(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	target := model.VmResources{CPU: 1, Mem: 128, Disk: 256}
	s.resizeErr = fmt.Errorf("growth outcome unknown")
	if _, err := r.ResizeVM(ctx, created.ID, target); err == nil {
		t.Fatal("resize failure hidden")
	}
	// The fake grows before reporting failure, exactly like a real resize
	// whose outcome was lost; the disk already carries the target size.
	vm, err := loadVM(ctx, r.db.conn, created.ID)
	if err != nil || vm.PendingResources == nil {
		t.Fatal(vm, err)
	}
	s.resizeErr = nil
	resizes := s.resizes
	if _, err := r.ResizeVM(ctx, created.ID, target); err != nil {
		t.Fatal(err)
	}
	if s.resizes != resizes {
		t.Fatal("same-size resize re-executed disk growth")
	}
	vm, err = loadVM(ctx, r.db.conn, created.ID)
	if err != nil || vm.PendingResources != nil || vm.CPU != 1 || vm.Mem != 128 {
		t.Fatal(vm, err)
	}
}

func TestFirecrackerResizeRetryFinishesFilesystemAtTargetDiskSize(t *testing.T) {
	r, _, s := runtimeFixture(t)
	ctx := context.Background()
	vm := &model.Vm{Backup: model.NewBackupState(), ID: "partial-ext4", EnvID: "env", HostID: "host", Image: "alpine", Engine: model.Firecracker, CPU: 1, Mem: 128, Disk: 128, State: model.VmStopped, Options: model.VmOptions{RequestedDisk: 128}}
	if err := saveVM(ctx, r.db.conn, vm); err != nil {
		t.Fatal(err)
	}
	s.filesystemSize = s.size
	s.resizeErr = fmt.Errorf("resize2fs failed after backing disk grew")
	target := model.VmResources{CPU: 1, Mem: 128, Disk: 256}
	// Both the original failure and a failed retry must retain the intent;
	// backing-disk capacity alone cannot prove filesystem completion.
	for range 2 {
		if _, err := r.ResizeVM(ctx, vm.ID, target); err == nil {
			t.Fatal("incomplete filesystem resize reported success")
		}
		stored, err := loadVM(ctx, r.db.conn, vm.ID)
		if err != nil || stored.PendingResources == nil || *stored.PendingResources != target || stored.Disk != 128 || s.size != 256*1024*1024 || s.filesystemSize != 128*1024*1024 {
			t.Fatal(stored, s, err)
		}
	}
	s.resizeErr = nil
	updated, err := r.ResizeVM(ctx, vm.ID, target)
	if err != nil || updated.PendingResources != nil || updated.Error != nil || updated.Disk != 256 || s.filesystemSize != 256*1024*1024 {
		t.Fatal(updated, s, err)
	}
	resizes := s.resizes
	if _, err := r.ResizeVM(ctx, vm.ID, target); err != nil || s.resizes != resizes {
		t.Fatal("completed resize was not idempotent", err)
	}
}
