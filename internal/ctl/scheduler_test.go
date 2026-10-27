package ctl

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/model"
)

func u32(n uint32) *uint32 { return &n }
func schedulerHost(id string, cpu, mem uint32, engines ...model.Engine) model.Host {
	h := hostRecord(id)
	h.Resource.CPUTotal = cpu
	h.Resource.MemTotal = mem
	h.Engines = engines
	h.ImageSizes = model.Map[string, uint32]{"ubuntu": 128}
	h.Capabilities = model.List[string]{"guest_config", "isolated_network", "firecracker_jailer", "firecracker_disk_resize", "firecracker_zvol"}
	return h
}
func schedulerSpec() api.VmSpec {
	return api.VmSpec{Image: "ubuntu", Engine: model.Qemu, CPU: u32(2), Mem: u32(1024), Disk: u32(40960), Ports: model.List[uint16]{22}}
}
func TestSchedulerBestFitAndErrorPriorities(t *testing.T) {
	spec := schedulerSpec()
	large, small := schedulerHost("large", 16, 32768, model.Qemu), schedulerHost("small", 4, 4096, model.Qemu)
	placement, err := PlaceVM([]model.Host{large, small}, spec, nil)
	if err != nil || placement.HostID != "small" {
		t.Fatal(placement, err)
	}
	offline := small
	offline.State = model.HostOffline
	for _, tc := range []struct {
		host    model.Host
		images  HostImages
		message string
	}{{offline, nil, "no online hosts"}, {schedulerHost("docker", 8, 4096, model.Docker), nil, "supports engine=qemu"}, {schedulerHost("full", 1, 512, model.Qemu), nil, "enough resources"}, {small, HostImages{"small": {"alpine": true}}, "has image 'ubuntu'"}} {
		if _, err := PlaceVM([]model.Host{tc.host}, spec, tc.images); err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Fatal(tc.message, err)
		}
	}
	matching := HostImages{"small": {"ubuntu": true}}
	if got, err := PlaceVM([]model.Host{small}, spec, matching); err != nil || got.HostID != "small" {
		t.Fatal(got, err)
	}
}
func TestVMStoragePrefersUsableZFSThenFallsBackToFiles(t *testing.T) {
	for _, kind := range []model.Engine{model.Qemu, model.Firecracker} {
		file, zfs := schedulerHost("file", 4, 4096, kind), schedulerHost("zfs", 8, 8192, kind)
		zfs.Storage = model.Zvol
		spec := schedulerSpec()
		spec.Engine = kind
		if got, err := PlaceVM([]model.Host{file, zfs}, spec, nil); err != nil || got.HostID != "zfs" {
			t.Fatal(got, err)
		}
		if got, err := PlaceVM([]model.Host{file, zfs}, spec, HostImages{"file": {"ubuntu": true}}); err != nil || got.HostID != "file" {
			t.Fatal(got, err)
		}
		zfs.Resource.CPUUsed = zfs.Resource.CPUTotal
		if got, err := PlaceVM([]model.Host{file, zfs}, spec, nil); err != nil || got.HostID != "file" {
			t.Fatal(got, err)
		}
		zfs.Resource.CPUUsed = 0
		zfs.State = model.HostOffline
		if got, err := PlaceVM([]model.Host{file, zfs}, spec, nil); err != nil || got.HostID != "file" {
			t.Fatal(got, err)
		}
	}
}
func TestDockerPlacementDoesNotPreferVMStorageOrCheckImageDirectory(t *testing.T) {
	file, zfs := schedulerHost("small", 4, 4096, model.Docker), schedulerHost("large", 8, 8192, model.Docker)
	zfs.Storage = model.Zvol
	spec := schedulerSpec()
	spec.Engine = model.Docker
	spec.Disk = nil
	got, err := PlaceVM([]model.Host{file, zfs}, spec, HostImages{"small": {"alpine": true}})
	if err != nil || got.HostID != "small" || got.Disk != 0 {
		t.Fatal(got, err)
	}
}
func TestImageSizedDisksAndConfigDrivesReservedDuringPlacement(t *testing.T) {
	host := schedulerHost("host", 8, 8192, model.Firecracker)
	host.ImageSizes["ubuntu"] = 1024
	host.Resource.DiskTotal = 2052
	spec := schedulerSpec()
	spec.Engine = model.Firecracker
	spec.Disk = nil
	spec.GuestConfig = model.GuestConfig{"app.json": "{}"}
	got, err := PlaceVM([]model.Host{host}, spec, nil)
	if err != nil || got.Disk != 1028 {
		t.Fatal(got, err)
	}
	if _, err := ScheduleEnv([]model.Host{host}, []api.VmSpec{spec, spec}, nil); err == nil {
		t.Fatal("configuration disks omitted from reservation")
	}
	spec.Disk = u32(512)
	if _, err := PlaceVM([]model.Host{host}, spec, nil); err == nil {
		t.Fatal("base disk shrunk")
	}
	host.ImageSizes = nil
	spec.Disk = nil
	if _, err := PlaceVM([]model.Host{host}, spec, nil); err == nil {
		t.Fatal("unknown base capacity admitted")
	}
}
func TestBhyvePlacementAndEgressRequireCapacityAndCapabilities(t *testing.T) {
	host := schedulerHost("freebsd", 4, 4096, model.Bhyve)
	host.Resource.DiskTotal = 200
	spec := schedulerSpec()
	spec.Engine = model.Bhyve
	spec.Disk = nil
	spec.DenyOutgoing = true
	if _, err := PlaceVM([]model.Host{host}, spec, nil); err == nil {
		t.Fatal("legacy egress admitted")
	}
	host.Capabilities = append(host.Capabilities, "bhyve_deny_outgoing")
	if got, err := PlaceVM([]model.Host{host}, spec, nil); err != nil || got.Disk != 128 {
		t.Fatal(got, err)
	}
	if _, err := ScheduleEnv([]model.Host{host}, []api.VmSpec{spec, spec}, nil); err == nil {
		t.Fatal("bhyve roots omitted")
	}
	spec.DenyOutgoing = false
	host.Capabilities = nil
	if _, err := PlaceVM([]model.Host{host}, spec, nil); err != nil {
		t.Fatal(err)
	}
	host.ImageSizes = nil
	if _, err := PlaceVM([]model.Host{host}, spec, nil); err == nil {
		t.Fatal("unknown bhyve capacity")
	}
	host.ImageSizes = model.Map[string, uint32]{spec.Image: 0}
	if _, err := PlaceVM([]model.Host{host}, spec, nil); err == nil {
		t.Fatal("empty bhyve disk")
	}
}
func TestFreeBSDPlacementRequiresMatchingEngineAndFileStorageForJail(t *testing.T) {
	linux := schedulerHost("linux", 8, 8192, model.Qemu)
	freebsd := schedulerHost("freebsd", 4, 4096, model.Bhyve, model.Jail)
	freebsd.Capabilities = nil
	spec := schedulerSpec()
	spec.Disk = nil
	for _, kind := range []model.Engine{model.Bhyve, model.Jail} {
		spec.Engine = kind
		if _, err := PlaceVM([]model.Host{linux}, spec, nil); err == nil {
			t.Fatal("wrong host engine accepted")
		}
		if got, err := PlaceVM([]model.Host{linux, freebsd}, spec, nil); err != nil || got.HostID != "freebsd" {
			t.Fatal(got, err)
		}
	}
	zvol := freebsd
	zvol.ID = "freebsd-zvol"
	zvol.Storage = model.Zvol
	if _, err := PlaceVM([]model.Host{zvol}, spec, nil); err == nil {
		t.Fatal("jail zvol admitted")
	}
	spec.Engine = model.Bhyve
	if got, err := PlaceVM([]model.Host{freebsd, zvol}, spec, nil); err != nil || got.HostID != zvol.ID {
		t.Fatal(got, err)
	}
}
func TestFirecrackerZvolSizingAndIsolationRequireUpgradedAgent(t *testing.T) {
	host := schedulerHost("legacy", 8, 8192, model.Firecracker)
	host.Storage = model.Zvol
	host.Capabilities = slices.DeleteFunc(host.Capabilities, func(s string) bool { return s == "firecracker_zvol" })
	spec := schedulerSpec()
	spec.Engine = model.Firecracker
	if _, err := PlaceVM([]model.Host{host}, spec, nil); err == nil {
		t.Fatal("legacy Firecracker zvol admitted")
	}
	file := schedulerHost("file", 4, 4096, model.Firecracker)
	if got, err := PlaceVM([]model.Host{host, file}, spec, nil); err != nil || got.HostID != "file" {
		t.Fatal(got, err)
	}
	host = schedulerHost("legacy", 8, 4096, model.Qemu, model.Firecracker)
	host.Capabilities = nil
	spec = schedulerSpec()
	spec.IsolatedNetwork = true
	if _, err := PlaceVM([]model.Host{host}, spec, nil); err == nil || !strings.Contains(err.Error(), "capabilities") {
		t.Fatal(err)
	}
	spec.IsolatedNetwork = false
	if _, err := PlaceVM([]model.Host{host}, spec, nil); err != nil {
		t.Fatal(err)
	}
}
func TestFirecrackerReservesVMMMemoryHeadroom(t *testing.T) {
	host := schedulerHost("small", 2, 256, model.Firecracker)
	spec := schedulerSpec()
	spec.Engine = model.Firecracker
	spec.Mem = u32(256)
	spec.Disk = nil
	if _, err := PlaceVM([]model.Host{host}, spec, nil); err == nil {
		t.Fatal("VMM memory omitted")
	}
	spec.Mem = u32(128)
	if _, err := PlaceVM([]model.Host{host}, spec, nil); err != nil {
		t.Fatal(err)
	}
}
func TestScheduleEnvironmentUsesShadowResourcesAndStableTieOrder(t *testing.T) {
	hosts := []model.Host{schedulerHost("h1", 4, 4096, model.Qemu), schedulerHost("h2", 4, 4096, model.Qemu)}
	before := append([]model.Host{}, hosts...)
	spec := schedulerSpec()
	placements, err := ScheduleEnv(hosts, []api.VmSpec{spec, spec, spec}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, item := range placements {
		ids = append(ids, item.Placement.HostID)
	}
	if !reflect.DeepEqual(ids, []string{"h1", "h1", "h2"}) || !reflect.DeepEqual(hosts, before) {
		t.Fatal(ids, hosts)
	}
	if _, err := ScheduleEnv(hosts[:1], []api.VmSpec{spec, spec, spec}, nil); err == nil {
		t.Fatal("capacity exceeded")
	}
	if result, err := ScheduleEnv(hosts, nil, nil); err != nil || len(result) != 0 {
		t.Fatal(result, err)
	}
}
