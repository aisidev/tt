package ctl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
)

func managedHost() model.Host {
	host := hostRecord("host")
	host.Images = model.List[string]{"ubuntu"}
	host.ImageSizes = model.Map[string, uint32]{"ubuntu": 16}
	host.StorageFeatures = &model.StorageFeatures{ProtocolVersion: 1, Volumes: true, Snapshots: true, DataDisks: true}
	return host
}
func TestManagedEnvironmentPlanIsAtomicAndReplayUsesOriginalIdentity(t *testing.T) {
	ctx := command.WithContext(context.Background(), command.Envelope{Subject: "workspace-g1", Generation: 1, Token: 1, Key: "create", Fingerprint: strings.Repeat("a", 64)})
	server := newServer(testDB(t), nil)
	host := managedHost()
	if err := server.db.PutHost(ctx, &host); err != nil {
		t.Fatal(err)
	}
	size := uint32(128)
	request := api.CreateEnvReq{ID: "workspace-g1", Owner: "owner", VMs: model.List[api.VmSpec]{{Image: "ubuntu", Engine: model.Qemu, Disk: &size, DurableRoot: true}}}
	first, failure := server.planEnvironment(ctx, request, 100, 0)
	if failure != nil {
		t.Fatal(failure)
	}
	if len(first) != 1 || first[0].request.RootVolumeID == "" || first[0].request.VolumeOwner != "owner" {
		t.Fatal(first)
	}
	root, err := server.db.GetVolume(ctx, first[0].request.RootVolumeID)
	if err != nil || root == nil || root.State != "creating" {
		t.Fatal(root, err)
	}
	before, err := server.db.GetHost(ctx, "host")
	if err != nil {
		t.Fatal(err)
	}
	if before.Resource.DiskUsed != root.ReservedMiB() {
		t.Fatal("managed root was not charged exactly once", before.Resource.DiskUsed, root.ReservedMiB())
	}
	replay, failure := server.planEnvironment(ctx, request, 200, 0)
	if failure != nil {
		t.Fatal(failure)
	}
	after, err := server.db.GetHost(ctx, "host")
	if err != nil {
		t.Fatal(err)
	}
	if len(replay) != 1 || replay[0].request.VmID != first[0].request.VmID || replay[0].request.RootVolumeID != root.ID || after.Resource.DiskUsed != before.Resource.DiskUsed {
		t.Fatal("replay changed placement or reservation", first, replay, before, after)
	}
}
func TestLegacyHostCannotReceiveManagedDiskOptions(t *testing.T) {
	host := managedHost()
	host.StorageFeatures = nil
	size := uint32(128)
	if _, err := PlaceVM([]model.Host{host}, api.VmSpec{Image: "ubuntu", Engine: model.Qemu, Disk: &size, DurableRoot: true}, HostImages{"host": {"ubuntu": true}}); err == nil {
		t.Fatal("unknown storage protocol accepted")
	}
	host.StorageFeatures = &model.StorageFeatures{ProtocolVersion: 99, Volumes: true}
	if _, err := PlaceVM([]model.Host{host}, api.VmSpec{Image: "ubuntu", Engine: model.Qemu, Disk: &size, DurableRoot: true}, nil); err == nil {
		t.Fatal("newer storage protocol accepted")
	}
}
func TestDiskObservationCannotEraseUnsettledGrowthReservation(t *testing.T) {
	known := model.Volume{ID: "disk-g1", HostID: "host", Owner: "owner", Generation: 1, Revision: 1, SizeMiB: 3, PendingSizeMiB: 3, Format: "raw", State: "growing", Phase: "growing"}
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actual := known
		actual.SizeMiB = 1
		actual.PendingSizeMiB = 0
		actual.State = "ready"
		actual.Phase = "ready"
		api.WriteJSON(w, 200, api.Success(actual))
	}))
	defer agent.Close()
	server := newServer(testDB(t), nil)
	host := managedHost()
	host.Addr = strings.TrimPrefix(agent.URL, "http://")
	ctx := context.Background()
	if err := server.db.PutHost(ctx, &host); err != nil {
		t.Fatal(err)
	}
	if err := server.db.PutVolume(ctx, &known); err != nil {
		t.Fatal(err)
	}
	observed, failure := server.getVolume(ctx, known.ID)
	if failure != nil {
		t.Fatal(failure)
	}
	if observed.SizeMiB != 3 || observed.PendingSizeMiB != 3 || observed.State != "growing" {
		t.Fatal("old observation released pending growth", observed)
	}
}
func TestDelayedCheckpointReadCannotReviveDeletionTombstone(t *testing.T) {
	known := model.VolumeSnapshot{ID: "point-g1", VolumeID: "disk-g1", HostID: "host", Generation: 1, Revision: 1, State: "ready", Phase: "ready", Source: model.Volume{ID: "disk-g1", Generation: 1, SizeMiB: 1}}
	entered := make(chan struct{})
	release := make(chan struct{})
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		api.WriteJSON(w, 200, api.Success(known))
	}))
	defer agent.Close()
	server := newServer(testDB(t), nil)
	host := managedHost()
	host.Addr = strings.TrimPrefix(agent.URL, "http://")
	ctx := context.Background()
	if err := server.db.PutHost(ctx, &host); err != nil {
		t.Fatal(err)
	}
	if err := server.db.PutVolumeSnapshot(ctx, &known); err != nil {
		t.Fatal(err)
	}
	done := make(chan model.VolumeSnapshot, 1)
	failures := make(chan *APIError, 1)
	go func() { point, failure := server.getVolumeSnapshot(ctx, known.ID); done <- point; failures <- failure }()
	<-entered
	deleted := known
	deleted.State = "deleted"
	deleted.Phase = "deleted"
	deleted.Revision = 2
	if _, err := withDB(server, func(db *DB) (model.Unit, error) { return model.Unit{}, db.PutVolumeSnapshot(ctx, &deleted) }); err != nil {
		t.Fatal(err)
	}
	close(release)
	if point := <-done; point.State != "deleted" {
		t.Fatal("late read revived checkpoint", point)
	}
	if failure := <-failures; failure != nil {
		t.Fatal(failure)
	}
}

func TestBatchPlacementReservesComposedRootFootprint(t *testing.T) {
	host := managedHost()
	host.Resource.DiskTotal = 375
	size := uint32(128)
	spec := api.VmSpec{Image: "ubuntu", Engine: model.Qemu, Disk: &size, DurableRoot: true}
	if _, err := ScheduleEnv([]model.Host{host}, []api.VmSpec{spec, spec}, nil); err == nil {
		t.Fatal("batch ignored root metadata and kernel envelope")
	}
	host.Resource.CPUUsed = host.Resource.CPUTotal
	host.Resource.MemUsed = host.Resource.MemTotal
	host.Resource.DiskTotal = 500
	spec.InitialStopped = true
	if _, err := ScheduleEnv([]model.Host{host}, []api.VmSpec{spec, spec}, nil); err != nil {
		t.Fatal("stopped provisioning spent running seats", err)
	}
}

func TestRootDiskHostQualificationRequiresEngineImageAndCapacity(t *testing.T) {
	host := managedHost()
	spec := model.VolumeSpec{Engine: model.Qemu, Image: "ubuntu", SizeMiB: 128}
	if !volumeEngineHost(host, spec, true) {
		t.Fatal("qualified root host refused")
	}
	if volumeEngineHost(host, model.VolumeSpec{Engine: model.Qemu, Image: "missing", SizeMiB: 128}, true) {
		t.Fatal("root disk placed on a host without its image")
	}
	if volumeEngineHost(host, model.VolumeSpec{Engine: model.Qemu, Image: "ubuntu", SizeMiB: 1}, true) {
		t.Fatal("undersized root admitted")
	}
	if !volumeEngineHost(host, model.VolumeSpec{Engine: model.Qemu, Image: "portable-source", SizeMiB: 128}, false) {
		t.Fatal("portable import incorrectly required an existing source image")
	}
	host.Engines = nil
	if volumeEngineHost(host, spec, false) {
		t.Fatal("unbootable archive placement admitted")
	}
}

func TestDeletePlanWithoutExecutorRowSettlesOnlyAfterSuccessfulDeleteACK(t *testing.T) {
	ctx := command.WithContext(context.Background(), command.Envelope{Subject: "planned-g1", Generation: 1, Token: 2, Key: "cleanup", Fingerprint: strings.Repeat("d", 64)})
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			if r.Header.Get("X-Compute-Command") == "" {
				t.Error("cleanup was not fenced")
			}
			api.WriteJSON(w, 200, api.Empty())
			return
		}
		api.WriteJSON(w, 404, api.Resp[model.Volume]{Failure: &api.BoundaryError{Class: "terminal", Code: "not_found", Message: "disk absent"}})
	}))
	defer agent.Close()
	server := newServer(testDB(t), nil)
	host := managedHost()
	host.Addr = strings.TrimPrefix(agent.URL, "http://")
	if err := server.db.PutHost(ctx, &host); err != nil {
		t.Fatal(err)
	}
	volume := model.Volume{ID: "planned-g1", HostID: host.ID, Owner: "owner", Generation: 1, SizeMiB: 16, Format: "raw", State: "creating", Phase: "reserved"}
	if err := server.db.PutVolume(ctx, &volume); err != nil {
		t.Fatal(err)
	}
	observed, failure := server.mutateVolume(ctx, volume.ID, "DELETE", "", nil)
	if failure != nil || observed.State != "deleted" {
		t.Fatal(observed, failure)
	}
}
