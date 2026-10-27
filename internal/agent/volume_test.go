package agent

import (
	"context"
	"errors"
	"fmt"
	"github.com/aisidev/tt/internal/command"
	"os"
	"strings"
	"testing"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/storage"
	"github.com/aisidev/tt/model"
)

func diskFixture(t *testing.T) *Runtime {
	t.Helper()
	r, _, _ := runtimeFixture(t)
	r.store = storage.FileStore{}
	if err := os.MkdirAll(r.runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	return r
}
func TestIndependentRawVolumeRetainsDataAndCapacity(t *testing.T) {
	r := diskFixture(t)
	ctx := context.Background()
	spec := model.VolumeSpec{ID: "data-g1", Generation: 1, SizeMiB: 1, Retain: true}
	v, err := r.CreateVolume(ctx, spec)
	if err != nil || v.State != "ready" {
		t.Fatal(v, err)
	}
	if r.resource.DiskUsed != 1 {
		t.Fatal("unattached disk was not charged", r.resource)
	}
	file, err := os.OpenFile(r.volumePath(v.ID), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteAt([]byte("retained-data"), 100); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err = r.CreateVolume(ctx, spec); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(r.volumePath(v.ID))
	if err != nil || string(raw[100:113]) != "retained-data" {
		t.Fatal("replay overwrote disk", err)
	}
	var typed *api.BoundaryError
	if err = r.DeleteVolume(ctx, v.ID); !errors.As(err, &typed) || typed.Code != "volume_retained" {
		t.Fatal("retention ignored", err)
	}
	keep := false
	if _, err = r.UpdateVolume(ctx, v.ID, model.VolumeUpdate{Retain: &keep}); err != nil {
		t.Fatal(err)
	}
	if err = r.DeleteVolume(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(r.volumePath(v.ID)); !os.IsNotExist(err) {
		t.Fatal("disk survived confirmed deletion", err)
	}
	if r.resource.DiskUsed != 0 {
		t.Fatal("disk reservation leaked", r.resource)
	}
}
func TestVolumeGrowthIsDurableAndCannotShrink(t *testing.T) {
	r := diskFixture(t)
	ctx := context.Background()
	v, err := r.CreateVolume(ctx, model.VolumeSpec{ID: "grow-g1", Generation: 1, SizeMiB: 1})
	if err != nil {
		t.Fatal(err)
	}
	target := uint32(3)
	v.SizeMiB = target
	v.State = "growing"
	v.Phase = "growing"
	if err = r.saveVolume(ctx, v); err != nil {
		t.Fatal(err)
	}
	if err = r.recount(ctx); err != nil || r.resource.DiskUsed != 3 {
		t.Fatal("unknown grow lost capacity", r.resource, err)
	}
	if _, err = r.UpdateVolume(ctx, v.ID, model.VolumeUpdate{SizeMiB: &target}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(r.volumePath(v.ID))
	if err != nil || info.Size() != 3<<20 {
		t.Fatal(info, err)
	}
	smaller := uint32(2)
	if _, err = r.UpdateVolume(ctx, v.ID, model.VolumeUpdate{SizeMiB: &smaller}); err == nil {
		t.Fatal("volume shrank")
	}
}
func TestUnpublishedVolumeRetryUsesJournalAndNeverAdoptsReadyBytes(t *testing.T) {
	r := diskFixture(t)
	ctx := context.Background()
	v := model.Volume{ID: "stage-g1", HostID: r.hostID, Generation: 1, SizeMiB: 1, Format: "raw", State: "creating", Phase: "copying"}
	if err := r.saveVolume(ctx, &v); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.volumePath(v.ID), []byte("partial unpublished stage"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateVolume(ctx, model.VolumeSpec{ID: v.ID, Generation: 1, SizeMiB: 1}); err != nil {
		t.Fatal(err)
	}
	// Missing data after availability is a fault, never a fresh clone opportunity.
	if err := os.Remove(r.volumePath(v.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateVolume(ctx, model.VolumeSpec{ID: v.ID, Generation: 1, SizeMiB: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.volumePath(v.ID)); !os.IsNotExist(err) {
		t.Fatal("ready disk silently recreated", err)
	}
}
func TestUnattachedVolumeAdmissionAndMalformedCatalogFailClosed(t *testing.T) {
	r := diskFixture(t)
	ctx := context.Background()
	r.resource.DiskTotal = 2
	if _, err := r.CreateVolume(ctx, model.VolumeSpec{ID: "one-g1", Generation: 1, SizeMiB: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateVolume(ctx, model.VolumeSpec{ID: "two-g1", Generation: 1, SizeMiB: 1}); err == nil {
		t.Fatal("unattached disk capacity overspent")
	}
	if _, err := r.db.conn.ExecContext(ctx, "UPDATE volumes SET data='{}'"); err != nil {
		t.Fatal(err)
	}
	if err := r.recount(ctx); err == nil {
		t.Fatal("malformed catalog treated as empty")
	}
}

func managedFixture(t *testing.T) (*Runtime, *fakeEngine, *fakeStore) {
	t.Helper()
	r, e, s := runtimeFixture(t)
	s.ImageStore = storage.FileStore{}
	if err := os.MkdirAll(r.runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	s.beforeClone = func() {
		if err := os.WriteFile(s.lastClone, []byte("original root bytes"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return r, e, s
}
func managedContext(subject string, token uint64) context.Context {
	return command.WithContext(context.Background(), command.Envelope{Subject: subject, Generation: 1, Token: token, Key: fmt.Sprint(token), Fingerprint: strings.Repeat("a", 64)})
}
func TestManagedRootSurvivesVMDeletionAndOfflineRebinding(t *testing.T) {
	r, e, store := managedFixture(t)
	request := requestFixture()
	request.EnvID = "workspace-g1"
	request.DurableRoot = true
	request.RootVolumeID = "root-g1"
	ctx := managedContext(request.EnvID, 1)
	vm, err := r.CreateVM(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if store.clones != 1 || !vm.Options.RootManaged || vm.Options.RootDiskCharge != 0 {
		t.Fatal(vm, store.clones)
	}
	expected := (model.Volume{Image: request.Image, SizeMiB: request.Disk, State: "ready"}).ReservedMiB()
	if r.resource.DiskUsed != expected {
		t.Fatal("root was double charged", r.resource.DiskUsed, expected)
	}
	if err = r.DestroyVM(managedContext(request.EnvID, 2), vm.ID); err != nil {
		t.Fatal(err)
	}
	root, err := r.volume(ctx, request.RootVolumeID)
	if err != nil || root.State != "ready" || root.AttachedTo != "" {
		t.Fatal(root, err)
	}
	if _, err = os.Stat(r.volumePath(root.ID)); err != nil {
		t.Fatal("VM deletion removed retained root", err)
	}
	request.VmID = "replacement"
	request.EnvID = "replacement-g1"
	request.DurableRoot = false
	request.InitialStopped = true
	vm, err = r.CreateVM(managedContext(request.EnvID, 3), request)
	if err != nil {
		t.Fatal(err)
	}
	if vm.State != model.VmStopped || store.clones != 1 || e.creates != 1 {
		t.Fatal("rebind recreated or started disk", vm, store.clones, e.creates)
	}
	if err = r.StartVM(context.Background(), vm.ID); err != nil {
		t.Fatal(err)
	}
	if e.lastPath != r.volumePath(root.ID)+"/disk" {
		t.Fatal("start lost disk locality", e.lastPath)
	}
}
func TestFailedManagedCreateResumesWithoutRecloningReadyRoot(t *testing.T) {
	r, e, store := managedFixture(t)
	request := requestFixture()
	request.EnvID = "workspace-g1"
	request.RootVolumeID = "root-g1"
	request.DurableRoot = true
	e.createErr = errors.New("interrupted launch")
	ctx := managedContext(request.EnvID, 1)
	if _, err := r.CreateVM(ctx, request); err == nil {
		t.Fatal("injected launch succeeded")
	}
	if err := os.WriteFile(r.volumePath(request.RootVolumeID), []byte("user data after uncertain launch"), 0600); err != nil {
		t.Fatal(err)
	}
	e.createErr = nil
	e.state = model.VmStopped
	vm, err := r.CreateVM(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(r.volumePath(request.RootVolumeID))
	if err != nil || string(raw) != "user data after uncertain launch" || store.clones != 1 || !vm.Options.CreationComplete {
		t.Fatal("recovery replaced ready data", string(raw), store.clones, vm, err)
	}
}

func TestCheckpointFreezeFencesStartsGrowthAndIndependentSnapshots(t *testing.T) {
	r, _, _ := managedFixture(t)
	request := requestFixture()
	request.EnvID = "workspace-g1"
	request.DurableRoot = true
	request.RootVolumeID = "root-g1"
	request.InitialStopped = true
	ctx := managedContext(request.EnvID, 1)
	vm, err := r.CreateVM(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := r.freeze(ctx, vm.ID, "recovery-g1")
	if err != nil || len(frozen.Volumes) != 1 {
		t.Fatal(frozen, err)
	}
	if err = r.StartVM(context.Background(), vm.ID); err == nil {
		t.Fatal("start bypassed checkpoint fence")
	}
	target := uint32(256)
	if _, err = r.UpdateVolume(ctx, request.RootVolumeID, model.VolumeUpdate{SizeMiB: &target}); err == nil {
		t.Fatal("growth bypassed checkpoint fence")
	}
	if err = r.DestroyVM(ctx, vm.ID); err == nil {
		t.Fatal("deletion bypassed checkpoint fence")
	}
	if _, err = r.SnapshotVolume(ctx, request.RootVolumeID, "independent-g1"); err == nil {
		t.Fatal("independent snapshot bypassed checkpoint fence")
	}
	group, _ := command.FromContext(ctx)
	group.Group = "recovery-g1"
	if _, err = r.SnapshotVolume(command.WithContext(ctx, group), request.RootVolumeID, "child-g1"); err != nil {
		t.Fatal(err)
	}
	replay, err := r.freeze(ctx, vm.ID, "recovery-g1")
	if err != nil || replay.VM.ID != frozen.VM.ID || len(replay.Volumes) != 1 {
		t.Fatal("freeze did not resume immutable disk set", replay, err)
	}
	if err = r.thaw(ctx, vm.ID, "another-g1"); err == nil {
		t.Fatal("unrelated checkpoint thawed an owner")
	}
	if err = r.thaw(ctx, vm.ID, "recovery-g1"); err != nil {
		t.Fatal(err)
	}
	if err = r.StartVM(context.Background(), vm.ID); err != nil {
		t.Fatal("completed checkpoint retained writer fence", err)
	}
}
