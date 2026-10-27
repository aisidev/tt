package agent

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"sync/atomic"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/capability"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/internal/engine"
	"github.com/aisidev/tt/internal/guestconfig"
	"github.com/aisidev/tt/internal/netcfg"
	"github.com/aisidev/tt/internal/sshboot"
	"github.com/aisidev/tt/internal/storage"
	"github.com/aisidev/tt/model"
)

type hostNetwork interface {
	SetupBridge(context.Context) error
	SetupNAT(context.Context) error
	Isolate(context.Context, string, string) error
	RemoveIsolation(context.Context, string) error
	TapExists(context.Context, string) (bool, error)
	CreateTap(context.Context, string, string) error
	DestroyTap(context.Context, string) error
	RemovePortForwards(context.Context, string) error
	AddPortForward(context.Context, uint16, string, uint16) error
	DenyOutgoing(context.Context, string) error
	AllowOutgoing(context.Context, string) error
}

// Runtime is serialized by the service mutation lock. Disk/tool backup work uses
// an immutable setup snapshot and a separate inherited writer lock outside it.
type Runtime struct {
	quarantineMiB                atomic.Uint32
	quarantinePinned             atomic.Bool
	observationUnsafe            atomic.Bool
	processDomain                string
	processEpochOnly             bool
	transferActive               map[string]bool
	exportReaders                map[string]int
	hostID                       string
	db                           *database
	engines                      []model.Engine
	probedCapabilities           []string
	store                        storage.ImageStore
	storage                      model.Storage
	imageDir, runtimeDir, runDir string
	resource                     model.Resource
	network                      hostNetwork
	networkReady                 bool
	containerRuntime             *engine.ContainerRuntime
	sshIngress                   *SshIngress
	portStart, portEnd           uint16
	engineFactory                func(model.Engine, *engine.ContainerRuntime) (engine.Engine, error)
	portAvailable                func(uint16) bool
	probe                        hostProbe
	backupEnabled                bool
	backupUnsupported            *string
	backupRunning                map[string]bool
	backupLocks                  string
	now                          func() uint64
}

func newRuntime(ctx context.Context, host string, backend model.Storage, imageDir, runtimeDir, dbPath string, resource model.Resource, stderr io.Writer) (*Runtime, error) {
	if backend == model.File {
		if err := os.MkdirAll(imageDir, 0777); err != nil {
			return nil, fmt.Errorf("create image_dir: %w", err)
		}
		if err := os.MkdirAll(runtimeDir, 0777); err != nil {
			return nil, fmt.Errorf("create runtime_dir: %w", err)
		}
	}
	if err := os.MkdirAll(model.RunDir, 0777); err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}
	d, err := openAgentDatabase(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	if stderr != nil {
		// Assign before the recovery scan below so unreadable-inventory notices
		// reach the caller's writer, not the process default.
		d.stderr = stderr
	}
	store, err := storage.New(backend)
	if err != nil {
		d.Close()
		return nil, err
	}
	probe := nativeProbe()
	engines := probe.engines(ctx, nil)
	r := &Runtime{hostID: host, db: d, engines: engines, probedCapabilities: probe.capabilities(ctx, engines, backend), store: store, storage: backend, imageDir: imageDir, runtimeDir: runtimeDir, runDir: model.RunDir, resource: resource, network: netcfg.New(), portStart: 20000, portEnd: 65535, engineFactory: engine.New, probe: probe, backupUnsupported: stringPointer("backup unsupported: backend has not been qualified"), backupRunning: map[string]bool{}, backupLocks: filepath.Dir(dbPath), now: unixNow, portAvailable: tcpPortAvailable}
	vms, _, err := loadRecoverableVMs(ctx, d.conn, d.stderr)
	if err != nil {
		d.Close()
		return nil, err
	}
	for _, vm := range vms {
		if vm.Engine != model.Docker && (vm.State == model.VmRunning || vm.State == model.VmPaused) && vm.Error == nil {
			vm.Error = stringPointer("network recovery pending")
			if err := saveVM(ctx, d.conn, &vm); err != nil {
				d.Close()
				return nil, err
			}
		}
	}
	if err := r.recount(ctx); err != nil {
		d.Close()
		return nil, err
	}
	return r, nil
}
func stringPointer(s string) *string { return &s }
func unixNow() uint64 {
	n := time.Now().Unix()
	if n < 0 {
		return 0
	}
	return uint64(n)
}
func tcpPortAvailable(port uint16) bool {
	listener, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return false
	}
	listener.Close()
	return true
}
func (r *Runtime) Close() error { return r.db.Close() }
func (r *Runtime) ConfigureContainerRuntime(ctx context.Context, requested *engine.ContainerRuntime) error {
	selected, err := r.db.bindContainerRuntime(ctx, requested, func() *engine.ContainerRuntime {
		if runtime.GOOS != "linux" {
			return nil
		}
		for _, choice := range []engine.ContainerRuntime{engine.DockerRuntime, engine.PodmanRuntime} {
			if r.probe.probe(ctx, choice.Command(), "--version") {
				return &choice
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.containerRuntime = selected
	r.engines = slices.DeleteFunc(r.engines, func(e model.Engine) bool { return e == model.Docker })
	if runtime.GOOS == "linux" && selected != nil && r.probe.probe(ctx, selected.Command(), "--version") {
		r.engines = append(r.engines, model.Docker)
	}
	return nil
}
func (r *Runtime) ConfigureNetwork(ctx context.Context, ingress *SshIngress, start, end uint16) error {
	if err := r.db.bindNetwork(ctx, ingress, start, end); err != nil {
		return err
	}
	r.sshIngress = ingress
	r.portStart = start
	r.portEnd = end
	return nil
}
func (r *Runtime) recount(ctx context.Context) error {
	vms, corrupt, err := loadRecoverableVMs(ctx, r.db.conn, r.db.stderr)
	if err != nil {
		return err
	}
	r.resource = ResourcesFor(r.resource, vms)
	disk, err := diskUsage(ctx, r.db.conn)
	if err != nil {
		return err
	}
	r.resource.DiskUsed = model.SatAdd(r.resource.DiskUsed, disk)
	r.applyQuarantine(&r.resource)
	if corrupt {
		pinToCapacity(&r.resource)
	}
	return nil
}

// pinToCapacity marks every reservation dimension as fully consumed; both the
// internal admission view and /api/info use it for unreadable inventory.
func pinToCapacity(r *model.Resource) {
	r.CPUUsed = r.CPUTotal
	r.MemUsed = r.MemTotal
	r.DiskUsed = r.DiskTotal
	r.VmCount = model.MaxVMs
}

func (r *Runtime) capabilityTags() []string {
	tags := append([]string{}, r.probedCapabilities...)
	tags = append(tags, capability.LegacyBackup)
	if r.backupUnsupported == nil {
		tag := capability.LegacyBackupReflink
		if r.storage == model.Zvol {
			tag = capability.LegacyBackupZvol
		}
		tags = append(tags, tag)
	}
	return tags
}
func (r *Runtime) capabilityReport() model.CapabilityReport {
	return capability.FromProbes(r.storage, r.engines, r.capabilityTags(), r.backupEnabled, r.backupUnsupported)
}
func (r *Runtime) AgentInfo() api.AgentInfo {
	report := r.capabilityReport()
	return api.AgentInfo{StorageFeatures: &model.StorageFeatures{ProtocolVersion: 1, ArchiveAbort: true, Archives: true, ImagePull: true, ImageCatalog: true, WorkspaceSnapshots: runtime.GOOS == "linux", Volumes: runtime.GOOS == "linux", Snapshots: runtime.GOOS == "linux", DataDisks: runtime.GOOS == "linux"}, CapabilityReport: &report, Capabilities: r.capabilityTags(), HostID: r.hostID, Resource: r.resource, Engines: r.engines, Storage: r.storage}
}
func (r *Runtime) ListVMs(ctx context.Context) ([]model.Vm, error) { return loadAllVMs(ctx, r.db.conn) }
func (r *Runtime) backupGuard(vm *model.Vm) error {
	if vm.Backup.Busy() || r.backupRunning[vm.ID] {
		return fmt.Errorf("conflict: backup operation unfinished; inspect it before lifecycle changes")
	}
	return nil
}
func (r *Runtime) clonePath(vm *model.Vm) string {
	if vm.Options.RootManaged {
		return r.volumePath(vm.Options.RootVolumeID)
	}
	return r.runtimeDir + "/clone-" + vm.ID
}
func (r *Runtime) imagePath(ctx context.Context, vm *model.Vm) (string, error) {
	if vm.Options.RootManaged && vm.Options.RootVolumeID == "" {
		return "", diskError("state", "root_detached", "root volume is detached")
	}
	path := r.clonePath(vm)
	switch vm.Engine {
	case model.Firecracker:
		return r.store.FirecrackerDir(ctx, path)
	case model.Qemu, model.Bhyve:
		return r.store.ResolveDisk(ctx, path)
	case model.Jail:
		return path, nil
	case model.Docker:
		return vm.Image, nil
	default:
		return "", fmt.Errorf("unknown engine: %s", vm.Engine)
	}
}
func (r *Runtime) ensureNetwork(ctx context.Context) error {
	if !r.networkReady {
		if err := r.network.SetupBridge(ctx); err != nil {
			return err
		}
		if err := r.network.SetupNAT(ctx); err != nil {
			return err
		}
		r.networkReady = true
	}
	return nil
}
func (r *Runtime) restoreNetwork(ctx context.Context, vm *model.Vm) error {
	if vm.Engine != model.Docker {
		if vm.Engine == model.Jail && vm.Options.DenyOutgoing {
			//lint:ignore ST1005 Preserve the pinned compatibility diagnostic.
			return fmt.Errorf("Jail does not support deny_outgoing; retained guest requires operator review")
		}
		if runtime.GOOS == "linux" && vm.Options.IsolatedNetwork {
			if err := r.network.Isolate(ctx, vm.ID, vm.IP); err != nil {
				return err
			}
		}
		if vm.Engine != model.Jail && (vm.State == model.VmRunning || vm.State == model.VmPaused) {
			present, err := r.network.TapExists(ctx, vm.ID)
			if err != nil {
				return err
			}
			if !present {
				return fmt.Errorf("live VM tap is missing; stop/start the VM to attach a new tap")
			}
		}
		if vm.Engine != model.Jail {
			if err := r.network.CreateTap(ctx, vm.ID, vm.IP); err != nil {
				return err
			}
		}
		if err := r.network.RemovePortForwards(ctx, vm.IP); err != nil {
			return err
		}
		ports := make([]int, 0, len(vm.PortMap))
		for guest := range vm.PortMap {
			ports = append(ports, int(guest))
		}
		sort.Ints(ports)
		for _, guest := range ports {
			if err := r.network.AddPortForward(ctx, vm.PortMap[uint16(guest)], vm.IP, uint16(guest)); err != nil {
				return err
			}
		}
		var err error
		if vm.Options.DenyOutgoing {
			err = r.network.DenyOutgoing(ctx, vm.IP)
		} else {
			err = r.network.AllowOutgoing(ctx, vm.IP)
		}
		if err != nil {
			return err
		}
	}
	if r.sshIngress != nil {
		return r.sshIngress.Apply(ctx, vm)
	}
	return nil
}
func normalizedOptions(o model.VmOptions) model.VmOptions {
	o.Ports = append(model.List[uint16]{}, o.Ports...)
	slices.Sort(o.Ports)
	o.Ports = slices.Compact(o.Ports)
	o.SSHKeys = append(model.List[string]{}, o.SSHKeys...)
	slices.Sort(o.SSHKeys)
	o.SSHKeys = slices.Compact(o.SSHKeys)
	return o
}
func bytesMiB(bytes uint64) uint64 { return bytes/(1024*1024) + boolUint64(bytes%(1024*1024) != 0) }
func boolUint64(value bool) uint64 {
	if value {
		return 1
	}
	return 0
}
func (r *Runtime) CreateVM(ctx context.Context, req api.CreateVmReq) (*model.Vm, error) {
	if r.processEpochOnly && req.Engine != model.Qemu && req.Engine != model.Firecracker {
		return nil, diskError("terminal", "unimplemented", "external process epoch scope accepts QEMU and Firecracker only")
	}
	if req.HostID != "" && req.HostID != r.hostID {
		return nil, diskError("state", "wrong_locality", "VM targets a different host")
	}

	if req.DurableRoot || req.RootVolumeID != "" || len(req.DataVolumeIDs) > 0 {
		if _, ok := command.FromContext(ctx); !ok {
			return nil, diskError("terminal", "missing_identity", "managed disk creation requires command identity")
		}
	}

	if err := model.ValidateName(req.VmID, "vm_id"); err != nil {
		return nil, err
	}
	if err := model.ValidateName(req.EnvID, "env_id"); err != nil {
		return nil, err
	}
	if err := model.ValidateImage(req.Image, req.Engine); err != nil {
		return nil, err
	}
	if err := guestconfig.Validate(req.Engine, req.GuestConfig, req.IsolatedNetwork); err != nil {
		return nil, err
	}
	var requestedDisk *uint32
	if req.Disk > 0 {
		requestedDisk = &req.Disk
	}
	if err := capability.ValidateVMOptions(req.Engine, requestedDisk, req.DenyOutgoing, req.SSHKeys, req.Ports); err != nil {
		return nil, err
	}
	if err := capability.Run.RequireDesign(req.Engine, r.storage); err != nil {
		return nil, err
	}
	if req.Engine == model.Qemu && req.Disk == 0 {
		return nil, fmt.Errorf("QEMU disk size must be > 0")
	}
	if req.CPU == 0 || req.Mem == 0 {
		return nil, fmt.Errorf("cpu and memory must be > 0")
	}
	ssh, err := sshboot.ResolveOptions(req.Engine, req.SSH, req.SSHKeys)
	if err != nil {
		return nil, err
	}
	if _, present := req.GuestConfig[sshboot.SeedFile]; present {
		return nil, fmt.Errorf("reserved guest configuration filename")
	}
	options := normalizedOptions(model.VmOptions{SSH: ssh, Ports: req.Ports, SSHKeys: req.SSHKeys, DenyOutgoing: req.DenyOutgoing, RequestedDisk: req.Disk, IsolatedNetwork: req.IsolatedNetwork, GuestConfigDigest: guestconfig.Digest(req.GuestConfig)})
	options.RootManaged = req.DurableRoot || req.RootVolumeID != ""
	options.RootVolumeID = req.RootVolumeID
	options.InitialStopped = req.InitialStopped
	if options.RootManaged && options.RootVolumeID == "" {
		return nil, diskError("terminal", "invalid_volume", "managed root requires an explicit volume identity")
	}
	if options.RootManaged && ssh != nil {
		return nil, diskError("terminal", "unimplemented", "managed root SSH bootstrap is not enabled")
	}
	if len(req.DataVolumeIDs) > 8 {
		return nil, diskError("terminal", "size_exceeded", "at most eight data volumes are supported")
	}
	seen := map[string]bool{}
	for _, id := range req.DataVolumeIDs {
		if seen[id] || id == req.RootVolumeID {
			return nil, diskError("terminal", "invalid_volume", "duplicate disk attachment")
		}
		seen[id] = true
		disk, err := r.dataAttachment(ctx, id)
		if err != nil {
			return nil, err
		}
		options.DataVolumes = append(options.DataVolumes, disk)
	}

	existing, err := loadVM(ctx, r.db.conn, req.VmID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if err := r.backupGuard(existing); err != nil {
			return nil, err
		}
		previous := normalizedOptions(existing.Options)
		previous.RootDiskCharge = 0
		previous.CreationComplete = false
		if existing.EnvID != req.EnvID || existing.Image != req.Image || existing.Engine != req.Engine || existing.CPU != req.CPU || existing.Mem != req.Mem || !reflect.DeepEqual(previous, options) {
			return nil, fmt.Errorf("VM ID already exists with different creation parameters")
		}
		if existing.Options.RootManaged && !existing.Options.CreationComplete && existing.State != model.VmDeleting {
			return r.resumeManagedCreation(ctx, existing, req)
		}
		if existing.State == model.VmFailed || existing.State == model.VmDeleting {
			return nil, fmt.Errorf("VM %s is %s; inspect and delete it before recreating", existing.ID, existing.State)
		}
		return existing, nil
	}
	create := capability.Create{Disk: req.Disk > 0, Ports: len(req.Ports) > 0, Keys: len(req.SSHKeys) > 0, SSH: ssh != nil, GuestConfig: len(req.GuestConfig) > 0, DenyOutgoing: req.DenyOutgoing, Isolated: req.IsolatedNetwork}
	for _, feature := range create.Requirements(req.Engine) {
		if err := capability.Require(r.capabilityReport(), req.Engine, r.storage, feature); err != nil {
			return nil, err
		}
	}
	if err := r.recount(ctx); err != nil {
		return nil, err
	}
	base := r.imageDir + "/" + req.Image
	var existingRoot *model.Volume
	if options.RootManaged {
		existingRoot, err = diskRecord[model.Volume](ctx, r.db.conn, "volumes", options.RootVolumeID)
		if err != nil {
			return nil, err
		}
		if existingRoot != nil {
			if existingRoot.State != "ready" && existingRoot.State != "attached" {
				return nil, diskError("state", "volume_busy", "root volume is unsettled")
			}
			if existingRoot.Engine != req.Engine || existingRoot.Image != req.Image || existingRoot.SizeMiB != req.Disk {
				return nil, diskError("terminal", "invalid_volume", "existing root must match image, engine and disk capacity")
			}
			if existingRoot.AttachedTo != "" && existingRoot.AttachedTo != req.VmID {
				return nil, diskError("state", "volume_attached", "root volume belongs to another VM")
			}
			base = r.volumePath(existingRoot.ID)
		} else if !req.DurableRoot {
			return nil, diskError("terminal", "not_found", "root volume not found")
		}
	}
	// An existing verified root owns all boot bytes. A portable restoration
	// must not require a source cache that may have died with another host.
	if existingRoot == nil && req.Engine != model.Docker {
		if err := r.imageImportGuard(ctx, req.Image); err != nil {
			return nil, err
		}
	}
	if req.Engine != model.Docker {
		present, err := r.store.ImageExists(ctx, base)
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, fmt.Errorf("base image does not exist")
		}
	}
	var disk uint32
	switch req.Engine {
	case model.Docker, model.Jail:
		disk = 0
	case model.Bhyve:
		size, err := r.store.BhyveSize(ctx, base)
		if err != nil {
			return nil, err
		}
		mib := bytesMiB(size)
		if mib > math.MaxUint32 {
			return nil, fmt.Errorf("bhyve disk too large")
		}
		disk = uint32(mib)
	case model.Firecracker:
		size, err := r.store.FirecrackerSize(ctx, base)
		if err != nil {
			return nil, err
		}
		mib := bytesMiB(size)
		if mib > math.MaxUint32 {
			return nil, fmt.Errorf("rootfs too large")
		}
		if req.Disk != 0 && uint64(req.Disk) < mib {
			return nil, fmt.Errorf("requested disk is smaller than the base image (%d MiB)", mib)
		}
		root := max(uint32(mib), req.Disk)
		overhead := options.ConfigDiskMiB(req.Engine)
		if root > math.MaxUint32-overhead {
			return nil, fmt.Errorf("rootfs and configuration disk are too large")
		}
		disk = root + overhead
	case model.Qemu:
		size, err := r.store.QemuSize(ctx, base)
		if err != nil {
			return nil, err
		}
		mib := bytesMiB(size)
		if uint64(req.Disk) < mib {
			return nil, fmt.Errorf("requested disk is smaller than the base image (%d MiB)", mib)
		}
		disk = req.Disk
	}
	additionalDisk := disk
	if existingRoot != nil {
		additionalDisk = options.ConfigDiskMiB(req.Engine)
	}
	if options.RootManaged && existingRoot == nil {
		options.RootDiskCharge = req.Disk
	}
	cpuCharge, memCharge := req.CPU, req.Engine.MemoryReservation(req.Mem)
	if req.InitialStopped {
		cpuCharge = 0
		memCharge = 0
	}
	if !r.resource.CanFit(cpuCharge, memCharge, additionalDisk) {
		return nil, fmt.Errorf("insufficient resources on host %s", r.hostID)
	}
	if r.resource.VmCount >= model.MaxVMs {
		return nil, fmt.Errorf("VM limit reached")
	}
	vms, err := r.ListVMs(ctx)
	if err != nil {
		return nil, err
	}
	usedIPs := map[string]bool{}
	for _, vm := range vms {
		usedIPs[vm.IP] = true
	}
	ip := ""
	if req.Engine != model.Docker {
		for i := uint32(0); i < 65000; i++ {
			candidate := netcfg.VMIP(i)
			if !usedIPs[candidate] {
				ip = candidate
				break
			}
		}
		if ip == "" {
			return nil, fmt.Errorf("IP address space exhausted")
		}
	}
	ports := append([]uint16{}, req.Ports...)
	if (req.Engine == model.Qemu || req.Engine == model.Bhyve || req.Engine == model.Jail || options.SSH != nil) && !slices.Contains(ports, 22) {
		ports = append(ports, 22)
	}
	portMap, err := allocatePorts(vms, ports, r.portStart, r.portEnd, r.portAvailable)
	if err != nil {
		return nil, err
	}
	vm := &model.Vm{Backup: model.NewBackupState(), ID: req.VmID, EnvID: req.EnvID, HostID: r.hostID, Image: req.Image, Engine: req.Engine, CPU: req.CPU, Mem: req.Mem, Disk: disk, IP: ip, PortMap: portMap, Options: options, State: model.VmCreating, CreatedAt: r.now()}
	if err := saveVM(ctx, r.db.conn, vm); err != nil {
		return nil, err
	}
	if err := r.recount(ctx); err != nil {
		return nil, err
	}
	if err := r.createResources(ctx, vm, req.GuestConfig, base, req.Disk, req); err != nil {
		vm.State = model.VmFailed
		vm.Error = stringPointer(err.Error())
		if saveErr := saveVM(ctx, r.db.conn, vm); saveErr != nil {
			return nil, saveErr
		}
		return nil, err
	}
	vm.State = model.VmRunning
	if req.InitialStopped {
		vm.State = model.VmStopped
	}
	if vm.Options.RootManaged {
		vm.Options.CreationComplete = true
	}
	if err := saveVM(ctx, r.db.conn, vm); err != nil {
		return nil, err
	}
	return vm, r.recount(ctx)
}
func (r *Runtime) createResources(ctx context.Context, vm *model.Vm, caller model.GuestConfig, base string, disk uint32, request ...api.CreateVmReq) error {
	config := model.GuestConfig{}
	for name, value := range caller {
		config[name] = value
	}
	if vm.Options.SSH != nil {
		settings := vm.Options.SSH
		script, key, err := sshboot.Bootstrap(ctx, *settings, vm.Options.SSHKeys)
		if err != nil {
			return err
		}
		if err := writeSSHSeed(filepath.Join(r.runDir, "ssh-"+vm.ID+".sh"), script); err != nil {
			return err
		}
		host := ""
		if r.sshIngress != nil {
			host = r.sshIngress.PublicAddress.String()
		}
		vm.SSH = &model.SshInfo{User: settings.User, Sudo: settings.Sudo, Host: host, Port: vm.PortMap[22], HostKey: key}
		if err := saveVM(ctx, r.db.conn, vm); err != nil {
			return err
		}
		if vm.Engine == model.Firecracker {
			config[sshboot.SeedFile] = script
		}
	}
	if vm.Options.RootManaged || len(vm.Options.DataVolumes) > 0 {
		if len(request) == 0 {
			return diskError("terminal", "missing_identity", "managed disk creation request is missing")
		}
		if err := r.prepareManagedVolumes(ctx, vm, request[0]); err != nil {
			return err
		}
	}
	if vm.Engine != model.Docker {
		if err := r.ensureNetwork(ctx); err != nil {
			return err
		}
		clone := r.clonePath(vm)
		if !vm.Options.RootManaged {
			if err := r.store.CloneImage(ctx, base, clone); err != nil {
				return err
			}
		}
		if runtime.GOOS == "linux" && vm.Engine == model.Firecracker {
			if disk > 0 && !vm.Options.RootManaged {
				if err := r.store.ResizeFirecracker(ctx, clone, disk); err != nil {
					return err
				}
			}
			directory, err := r.store.FirecrackerDir(ctx, clone)
			if err != nil {
				return err
			}
			if vm.Options.SSH != nil {
				if err := sshboot.PreparedRootfs(ctx, filepath.Join(directory, "rootfs.ext4")); err != nil {
					return err
				}
			}
			if err := guestconfig.WriteDisk(ctx, directory, config); err != nil {
				return err
			}
		}
		if vm.Engine == model.Qemu && !vm.Options.RootManaged {
			if err := r.store.ResizeDisk(ctx, clone, vm.Disk); err != nil {
				return err
			}
		}
		if err := r.restoreNetwork(ctx, vm); err != nil {
			return err
		}
	}
	if vm.Options.InitialStopped {
		return nil
	}
	path, err := r.imagePath(ctx, vm)
	if err != nil {
		return err
	}
	eng, err := r.engineFactory(vm.Engine, r.containerRuntime)
	if err != nil {
		return err
	}
	return eng.Create(ctx, vm, path, r.store.DiskFormat(), vm.Options.SSHKeys)
}
func writeSSHSeed(path, script string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".tt-ssh-seed-*")
	if err != nil {
		return fmt.Errorf("SSH seed staging: %w", err)
	}
	defer file.Close()
	defer os.Remove(file.Name())
	if _, err := file.WriteString(script); err != nil {
		return fmt.Errorf("write initial SSH seed: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync initial SSH seed: %w", err)
	}
	return os.Rename(file.Name(), path)
}
