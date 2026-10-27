package ctl

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/capability"
	"github.com/aisidev/tt/internal/guestconfig"
	"github.com/aisidev/tt/internal/sshboot"
	"github.com/aisidev/tt/model"
)

type Placement struct {
	HostID, HostAddr string
	Disk             uint32
}
type ScheduledVM struct {
	Spec      api.VmSpec
	Placement Placement
}
type HostImages map[string]map[string]bool

func diskReservation(spec api.VmSpec, host model.Host) (uint32, bool) {
	if spec.Engine == model.Docker || spec.Engine == model.Jail {
		return 0, true
	}
	base, known := host.ImageSizes[spec.Image]
	disk := spec.Engine.DefaultDisk()
	if spec.Disk != nil {
		disk = *spec.Disk
	} else if spec.Engine == model.Firecracker || spec.Engine == model.Bhyve {
		if !known || base == 0 {
			return 0, false
		}
		disk = base
	}
	if known && disk < base && spec.RootVolumeID == "" {
		return 0, false
	}
	overhead := guestconfig.DiskMiB(spec.Engine, len(spec.GuestConfig) > 0, spec.SSH != nil)
	if disk > math.MaxUint32-overhead {
		return 0, false
	}
	return disk + overhead, true
}
func defaultValue(value *uint32, fallback uint32) uint32 {
	if value != nil {
		return *value
	}
	return fallback
}
func createRequirements(spec api.VmSpec) ([]capability.Feature, error) {
	ssh, err := sshboot.ResolveOptions(spec.Engine, spec.SSH, spec.SSHKeys)
	if err != nil {
		return nil, err
	}
	required := capability.Create{Disk: spec.Disk != nil, Ports: len(spec.Ports) > 0, Keys: len(spec.SSHKeys) > 0, SSH: ssh != nil, GuestConfig: len(spec.GuestConfig) > 0, DenyOutgoing: spec.DenyOutgoing, Isolated: spec.IsolatedNetwork}
	return required.Requirements(spec.Engine), nil
}
func PlaceVM(hosts []model.Host, spec api.VmSpec, images HostImages) (Placement, error) {
	cpu := defaultValue(spec.CPU, model.VMCPUDefault)
	mem := spec.Engine.MemoryReservation(defaultValue(spec.Mem, model.VMMemDefault))
	if spec.InitialStopped {
		cpu = 0
		mem = 0
	}
	requestedDisk := defaultValue(spec.Disk, spec.Engine.DefaultDisk())
	checkImages := len(images) > 0 && spec.Engine != model.Docker && spec.RootVolumeID == ""
	requirements, err := createRequirements(spec)
	if err != nil {
		return Placement{}, err
	}
	supports := func(host model.Host) bool {
		managed := spec.DurableRoot || spec.RootVolumeID != "" || len(spec.DataVolumeIDs) > 0 || spec.InitialStopped
		return capability.RequireHost(host, spec.Engine, requirements) == nil && (!managed || host.StorageFeatures.SupportsVolumes())
	}
	candidates := []model.Host{}
	for _, host := range hosts {
		disk, ok := diskReservation(spec, host)
		if spec.HostID != "" && spec.HostID != host.ID {
			continue
		}
		if host.State == model.HostOnline && supports(host) && slices.Contains(host.Engines, spec.Engine) && ok && host.Resource.CanFit(cpu, mem, placementDiskCharge(spec, disk)) && (!checkImages || images[host.ID][spec.Image]) {
			candidates = append(candidates, host)
		}
	}
	if len(candidates) == 0 {
		online, withEngine, withCapabilities, withResources := 0, 0, 0, 0
		badDisk := false
		reasons := []string{}
		for _, host := range hosts {
			if host.State != model.HostOnline {
				continue
			}
			online++
			if !slices.Contains(host.Engines, spec.Engine) {
				continue
			}
			withEngine++
			if err := capability.RequireHost(host, spec.Engine, requirements); err != nil {
				reasons = append(reasons, host.ID+": "+err.Error())
				continue
			}
			withCapabilities++
			disk, ok := diskReservation(spec, host)
			if !ok {
				badDisk = true
			} else if host.Resource.CanFit(cpu, mem, placementDiskCharge(spec, disk)) {
				withResources++
			}
		}
		if withEngine > 0 && withCapabilities == 0 {
			return Placement{}, fmt.Errorf("no online agent supports the requested capabilities: %s", strings.Join(reasons, "; "))
		}
		if badDisk {
			return Placement{}, fmt.Errorf("base image size is unavailable or exceeds requested disk; upgrade the agent or choose a sufficient explicit disk size")
		}
		if online == 0 {
			return Placement{}, fmt.Errorf("no online hosts available")
		}
		if withEngine == 0 {
			return Placement{}, fmt.Errorf("no online host supports engine=%s", spec.Engine)
		}
		if withResources == 0 {
			return Placement{}, fmt.Errorf("no host has enough resources for engine=%s, cpu=%d, mem=%dMB, disk=%dMB", spec.Engine, cpu, mem, requestedDisk)
		}
		return Placement{}, fmt.Errorf("no host has image '%s' for engine=%s", spec.Image, spec.Engine)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if spec.Engine != model.Docker && (a.Storage == model.Zvol) != (b.Storage == model.Zvol) {
			return a.Storage == model.Zvol
		}
		return a.Resource.MemFree() < b.Resource.MemFree()
	})
	host := candidates[0]
	disk, ok := diskReservation(spec, host)
	if !ok {
		return Placement{}, fmt.Errorf("invalid disk reservation")
	}
	return Placement{HostID: host.ID, HostAddr: host.Addr, Disk: disk}, nil
}
func ScheduleEnv(hosts []model.Host, specs []api.VmSpec, images HostImages) ([]ScheduledVM, error) {
	shadow := append([]model.Host{}, hosts...)
	result := make([]ScheduledVM, 0, len(specs))
	for _, spec := range specs {
		placement, err := PlaceVM(shadow, spec, images)
		if err != nil {
			return nil, err
		}
		for i := range shadow {
			host := &shadow[i]
			if host.ID == placement.HostID {
				// Reservation arithmetic saturates everywhere, including the
				// speculative pass, per the compatibility contract.
				if !spec.InitialStopped {
					host.Resource.CPUUsed = model.SatAdd(host.Resource.CPUUsed, defaultValue(spec.CPU, model.VMCPUDefault))
					host.Resource.MemUsed = model.SatAdd(host.Resource.MemUsed, spec.Engine.MemoryReservation(defaultValue(spec.Mem, model.VMMemDefault)))
				}
				host.Resource.DiskUsed = model.SatAdd(host.Resource.DiskUsed, placementDiskCharge(spec, placement.Disk))
				host.Resource.VmCount = model.SatAdd(host.Resource.VmCount, 1)
				break
			}
		}
		result = append(result, ScheduledVM{Spec: spec, Placement: placement})
	}
	return result, nil
}

func placementDiskCharge(spec api.VmSpec, logical uint32) uint32 {
	overhead := guestconfig.DiskMiB(spec.Engine, len(spec.GuestConfig) > 0, spec.SSH != nil)
	if spec.RootVolumeID != "" {
		return overhead
	}
	if spec.DurableRoot {
		return model.SatAdd((model.Volume{Image: spec.Image, SizeMiB: model.SatSub(logical, overhead), State: "creating"}).ReservedMiB(), overhead)
	}
	return logical
}
