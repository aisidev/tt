package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/backup"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

type application struct {
	ctx            context.Context
	stdout, stderr io.Writer
	now            func() uint64
	sleep          func(context.Context, time.Duration) error
	configPath     func() (string, error)
}

func newApplication(stdout, stderr io.Writer) *application {
	return &application{ctx: context.Background(), stdout: stdout, stderr: stderr, now: func() uint64 {
		now := time.Now().Unix()
		if now < 0 {
			return 0
		}
		return uint64(now)
	}, sleep: func(ctx context.Context, d time.Duration) error {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}, configPath: configPath}
}
func (a *application) out(format string, args ...any) { fmt.Fprintf(a.stdout, format+"\n", args...) }
func (a *application) diagnostic(format string, args ...any) {
	fmt.Fprintf(a.stderr, format+"\n", args...)
}
func pretty(value any) (string, error) {
	data, err := model.SerdeJSON(value)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		return "", err
	}
	return out.String(), nil
}
func debugName(value string) string {
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}
func debugEngines(engines []model.Engine) string {
	names := []string{}
	for _, engine := range engines {
		names = append(names, debugName(string(engine)))
	}
	return "[" + strings.Join(names, ", ") + "]"
}
func portKeys(ports model.Map[uint16, uint16]) []int {
	keys := make([]int, 0, len(ports))
	for key := range ports {
		keys = append(keys, int(key))
	}
	sort.Ints(keys)
	return keys
}
func debugPorts(ports model.Map[uint16, uint16]) string {
	entries := []string{}
	for _, key := range portKeys(ports) {
		entries = append(entries, fmt.Sprintf("%d: %d", key, ports[uint16(key)]))
	}
	return "{" + strings.Join(entries, ", ") + "}"
}
func expiryLabel(expires, now uint64) string {
	if expires == 0 {
		return "never (retained until explicit deletion)"
	}
	if expires <= now {
		return "expired; deletion pending"
	}
	remaining := expires - now
	return fmt.Sprintf("in %dh %dm %ds (deletes disks)", remaining/3600, remaining%3600/60, remaining%60)
}
func (a *application) networkCommand(c *Client, p parsed) error {
	switch p.path {
	case "status":
		status, err := fetch[api.FleetStatus](a.ctx, c, "GET", "/api/status", nil)
		if err != nil {
			return err
		}
		a.out("Fleet Status")
		a.out("  Hosts:   %d/%d online", status.HostsOnline, status.Hosts)
		a.out("  VMs:     %d", status.TotalVMs)
		a.out("  Envs:    %d", status.TotalEnvs)
		a.out("  CPU:     %d/%d cores", status.CPUUsed, status.CPUTotal)
		a.out("  Memory:  %d/%d MB", status.MemUsed, status.MemTotal)
		a.out("  Disk:    %d/%d MB", status.DiskUsed, status.DiskTotal)
	case "capabilities":
		matrix, err := fetch[model.Matrix](a.ctx, c, "GET", "/api/capabilities", nil)
		if err != nil {
			return err
		}
		if p.has("json") {
			text, err := pretty(matrix)
			if err != nil {
				return err
			}
			a.out("%s", text)
			return nil
		}
		a.out("%-13s %-7s %-23s CAPABILITIES", "ENGINE", "STORAGE", "PLATFORM")
		for _, entry := range matrix.Combinations {
			platform := entry.Platform
			tags := []string{}
			for _, feature := range entry.Features {
				if feature.Supported {
					tags = append(tags, feature.Tag)
				}
			}
			enabled := strings.Join(tags, ", ")
			if enabled == "" {
				enabled = "unsupported"
			}
			// Rust's custom enum Display implementations do not apply field padding.
			a.out("%s %s %-23s %s", entry.Engine, entry.Storage, platform, enabled)
		}
		a.out("Host prerequisites and VM eligibility are checked separately. Use 'tt host show ID' for the current host report.")
		a.out("FreeBSD bhyve and Jail are validated only on FreeBSD 15.x.")
	case "host add":
		host, err := fetch[model.Host](a.ctx, c, "POST", "/api/hosts", api.RegisterHostReq{Addr: p.argument})
		if err != nil {
			return err
		}
		a.out("Host registered: %s (%s)", host.ID, host.Addr)
		a.out("  Engines: %s", debugEngines(host.Engines))
		a.out("  Storage: %s", host.Storage)
		a.out("  Resources: %d CPU, %d MB RAM, %d MB disk", host.Resource.CPUTotal, host.Resource.MemTotal, host.Resource.DiskTotal)
	case "host list":
		hosts, err := fetch[model.List[model.Host]](a.ctx, c, "GET", "/api/hosts", nil)
		if err != nil {
			return err
		}
		if len(hosts) == 0 {
			a.out("No hosts registered.")
			return nil
		}
		a.out("%-12s %-22s %-8s %6s %8s %8s", "ID", "ADDR", "STATE", "CPU", "MEM(MB)", "VMs")
		for _, host := range hosts {
			a.out("%-12s %-22s %-8s %3d/%-3d %4d/%-4d %4d", host.ID, host.Addr, host.State, host.Resource.CPUUsed, host.Resource.CPUTotal, host.Resource.MemUsed, host.Resource.MemTotal, host.Resource.VmCount)
		}
	case "host show":
		host, err := fetch[model.Host](a.ctx, c, "GET", "/api/hosts/"+p.argument, nil)
		if err != nil {
			return err
		}
		a.printHost(host)
	case "host remove":
		if err := c.action(a.ctx, "DELETE", "/api/hosts/"+p.argument); err != nil {
			return err
		}
		a.out("Host removed: %s", p.argument)
	case "host detach":
		orphans, err := fetch[model.List[model.Vm]](a.ctx, c, "POST", "/api/hosts/"+p.argument+"/detach", model.Unit{})
		if err != nil {
			return err
		}
		text, err := pretty(orphans)
		if err != nil {
			return err
		}
		a.out("%s", text)
		a.diagnostic("Host %s detached. Listed guests may still be running; reclaim them on that host separately.", p.argument)
	case "env create":
		return a.createEnvironment(c, p)
	case "env list":
		envs, err := fetch[model.List[model.Env]](a.ctx, c, "GET", "/api/envs", nil)
		if err != nil {
			return err
		}
		if len(envs) == 0 {
			a.out("No environments.")
			return nil
		}
		a.out("%-16s %-12s %-8s %4s  EXPIRES", "NAME", "OWNER", "STATE", "VMs")
		for _, env := range envs {
			a.out("%-16s %-12s %-8s %4d  %s", env.ID, env.Owner, env.State, len(env.VmIds), expiryLabel(env.ExpiresAt, a.now()))
		}
	case "env show":
		detail, err := fetch[api.EnvDetail](a.ctx, c, "GET", "/api/envs/"+p.argument, nil)
		if err != nil {
			return err
		}
		a.printEnvironment(c, detail)
	case "env delete", "env stop", "env start":
		action := strings.TrimPrefix(p.path, "env ")
		path := "/api/envs/" + p.argument
		method := "DELETE"
		label := "deleted"
		if action != "delete" {
			method = "POST"
			path += "/" + action
			label = "stopped"
			if action == "start" {
				label = "started"
			}
		}
		if err := c.action(a.ctx, method, path); err != nil {
			return err
		}
		a.out("Environment %s: %s", label, p.argument)
	case "env resize":
		if err := model.ValidateName(p.argument, "vm_id"); err != nil {
			return err
		}
		vm, err := fetch[model.Vm](a.ctx, c, "POST", "/api/vms/"+p.argument+"/resources", model.VmResources{CPU: uint32(p.number("cpu")), Mem: uint32(p.number("mem")), Disk: uint32(p.number("disk"))})
		if err != nil {
			return err
		}
		a.out("VM %s updated and stopped: %d vCPU, %d MiB memory, %d MiB root disk", vm.ID, vm.CPU, vm.Mem, vm.Options.RequestedDisk)
	case "env backup show", "env backup create", "env backup restore", "env backup delete":
		return a.backupCommand(c, p)
	case "image list":
		images, err := fetch[model.List[api.ImageInfo]](a.ctx, c, "GET", "/api/images", nil)
		if err != nil {
			return err
		}
		if len(images) == 0 {
			a.out("No images available.")
			return nil
		}
		a.out("%-30s %-12s", "IMAGE", "HOST")
		for _, image := range images {
			a.out("%-30s %-12s", image.Name, image.HostID)
		}
	default:
		return fmt.Errorf("unknown command: %s", p.path)
	}
	return nil
}
func (a *application) printHost(host model.Host) {
	a.out("Host: %s", host.ID)
	a.out("  Address:  %s", host.Addr)
	a.out("  State:    %s", debugName(string(host.State)))
	a.out("  Engines:  %s", debugEngines(host.Engines))
	a.out("  Storage:  %s", host.Storage)
	a.out("  CPU:      %d/%d", host.Resource.CPUUsed, host.Resource.CPUTotal)
	a.out("  Memory:   %d/%d MB", host.Resource.MemUsed, host.Resource.MemTotal)
	a.out("  Disk:     %d/%d MB", host.Resource.DiskUsed, host.Resource.DiskTotal)
	a.out("  VMs:      %d", host.Resource.VmCount)
	if report := host.CapabilityReport; report != nil {
		suffix := ""
		if host.State == model.HostOffline {
			suffix = " (last known; host offline)"
		}
		a.out("  Capability report: v%d%s", report.Version, suffix)
		admission := "disabled"
		if report.BackupAdmission {
			admission = "enabled"
		}
		a.out("  Backup admission: %s", admission)
		for _, entry := range report.Engines {
			present := false
			for _, kind := range host.Engines {
				present = present || kind == entry.Engine
			}
			if !present {
				continue
			}
			tags := []string{}
			for _, feature := range entry.Features {
				if feature.Enabled {
					tags = append(tags, feature.Tag)
				}
			}
			a.out("    %s: %s", entry.Engine, strings.Join(tags, ", "))
			for _, feature := range entry.Features {
				if !feature.Enabled {
					reason := "not enabled"
					if feature.Reason != nil {
						reason = *feature.Reason
					}
					a.out("      %s: %s", feature.Tag, reason)
				}
			}
		}
	} else {
		a.out("  Legacy capabilities: %s", strings.Join(host.Capabilities, ", "))
	}
}
func (a *application) printEnvironment(c *Client, detail api.EnvDetail) {
	a.out("Environment: %s", detail.Env.ID)
	a.out("  Owner:   %s", detail.Env.Owner)
	a.out("  State:   %s", debugName(string(detail.Env.State)))
	a.out("  VMs:     %d", len(detail.VMs))
	a.out("  Expiry:  %s", expiryLabel(detail.Env.ExpiresAt, a.now()))
	for _, warning := range detail.Warnings {
		a.diagnostic("  warning: %s", warning)
	}
	a.out("")
	if len(detail.VMs) > 0 {
		idWidth, imageWidth := 2, 5
		for _, vm := range detail.VMs {
			idWidth = max(idWidth, len(vm.ID))
			imageWidth = max(imageWidth, len(vm.Image))
		}
		a.out("  %-*s %-*s %-10s %-8s %-16s PORTS", idWidth, "ID", imageWidth, "IMAGE", "ENGINE", "STATE", "IP")
		for _, vm := range detail.VMs {
			ports := []string{}
			for _, guest := range portKeys(vm.PortMap) {
				ports = append(ports, fmt.Sprintf("%d->%d", vm.PortMap[uint16(guest)], guest))
			}
			a.out("  %-*s %-*s %-10s %-8s %-16s %s", idWidth, vm.ID, imageWidth, vm.Image, vm.Engine, vm.State, vm.IP, strings.Join(ports, ", "))
			if target := vm.PendingResources; target != nil {
				a.out("    Pending resources: %d vCPU, %d MiB memory, %d MiB root disk; retry this target before starting", target.CPU, target.Mem, target.Disk)
			}
			if vm.SSH != nil && vm.SSH.ObservationError != nil {
				a.out("    SSH observation: %s", *vm.SSH.ObservationError)
			}
			if vm.Backup.HasArtifacts() || vm.Backup.LastResult != nil {
				current := "none"
				if vm.Backup.Current != nil {
					current = vm.Backup.Current.ID
				}
				a.out("    Backup: %s (%d retired, %d MiB reserved)", current, len(vm.Backup.Retired), vm.Backup.ReservedMiB())
				if pending := vm.Backup.Pending; pending != nil {
					message := "pending"
					if pending.Error != nil {
						message = *pending.Error
					}
					a.out("    Backup operation: %s %s; %s", pending.Request.OperationID, debugName(string(pending.Request.Action)), message)
				}
				if pending := vm.Backup.Forwarding; pending != nil {
					a.out("    Backup outcome unresolved: %s", pending.OperationID)
				}
				if vm.Backup.CleanupError != nil {
					a.out("    Backup cleanup: %s", *vm.Backup.CleanupError)
				}
			}
		}
	}
	a.printAccess(c, detail.VMs)
}
func (a *application) printAccess(c *Client, vms []model.Vm) {
	hosts, err := fetch[model.List[model.Host]](a.ctx, c, "GET", "/api/hosts", nil)
	if err != nil {
		a.diagnostic("  Cannot resolve host access addresses: %v", err)
		hosts = nil
	}
	for _, vm := range vms {
		if ssh := vm.SSH; ssh != nil {
			if ssh.Host == "" {
				a.out("  SSH %s: public endpoint not configured; user %s", vm.ID, ssh.User)
			} else {
				state := "SSH not yet ready from agent"
				if ssh.Ready {
					state = "SSH ready from agent; external routing must be reachable"
				}
				a.out("  SSH %s: ssh -p %d %s@%s (%s)", vm.ID, ssh.Port, ssh.User, ssh.Host, state)
			}
		} else if vm.Options.SSH != nil {
			a.out("  SSH %s: initial provisioning incomplete; inspect the VM error", vm.ID)
		}
		ports := portKeys(vm.PortMap)
		if vm.Engine != model.Docker && vm.IP != "" {
			for _, guest := range ports {
				a.out("  On agent host %s: %s:%d (direct guest TCP)", vm.HostID, vm.IP, guest)
			}
		}
		for _, host := range hosts {
			if host.ID != vm.HostID {
				continue
			}
			parsed, err := url.Parse("http://" + host.Addr)
			if err != nil || parsed.Hostname() == "" {
				break
			}
			address := parsed.Hostname()
			display := address
			if strings.Contains(address, ":") {
				display = "[" + address + "]"
			}
			for _, guest := range ports {
				if guest == 22 && (vm.SSH != nil || vm.Options.SSH != nil) {
					continue
				}
				port := vm.PortMap[uint16(guest)]
				if vm.Engine == model.Docker {
					a.out("  Access %s: %s:%d -> guest TCP %d", vm.ID, display, port, guest)
				} else {
					ip := net.ParseIP(address)
					if !strings.EqualFold(address, "localhost") && (ip == nil || !ip.IsLoopback()) {
						a.out("  From another machine, if reachable: %s:%d -> %s TCP %d", display, port, vm.ID, guest)
					}
				}
			}
			break
		}
	}
}
func (a *application) backupCommand(c *Client, p parsed) error {
	if err := model.ValidateName(p.argument, "vm_id"); err != nil {
		return err
	}
	command := strings.TrimPrefix(p.path, "env backup ")
	if command == "show" {
		view, err := fetch[api.BackupView](a.ctx, c, "GET", "/api/vms/"+p.argument+"/backup", nil)
		if err != nil {
			return err
		}
		text, err := pretty(view)
		if err != nil {
			return err
		}
		a.out("%s", text)
		return nil
	}
	expected := p.value("expected-revision")
	if !p.has("expected-revision") {
		view, err := fetch[api.BackupView](a.ctx, c, "GET", "/api/vms/"+p.argument+"/backup", nil)
		if err != nil {
			return err
		}
		if view.Vm.Backup.Busy() {
			return fmt.Errorf("backup operation unfinished; use its exact operation ID and expected revision")
		}
		expected = view.Vm.Backup.Revision
	}
	operation := p.value("operation-id")
	if !p.has("operation-id") {
		operation = uuid.NewString()
	}
	request := model.BackupRequest{OperationID: operation, ExpectedRevision: expected, Action: model.BackupAction(command)}
	generationArg := ""
	if p.has("generation") {
		generation := p.value("generation")
		request.Generation = &generation
		generationArg = " --generation " + generation
	}
	if err := backup.Validate(request); err != nil {
		return err
	}
	a.diagnostic("Exact retry after an unknown outcome:\n  tt env backup %s %s --operation-id %s --expected-revision %s%s", command, p.argument, operation, expected, generationArg)
	if request.Action == model.BackupRestore {
		a.diagnostic("Restoring disk generation %s; the VM stays stopped.", *request.Generation)
	}
	view, err := c.backup(a.ctx, p.argument, request)
	if err != nil {
		return err
	}
	text, err := pretty(view)
	if err != nil {
		return err
	}
	a.out("%s", text)
	return nil
}
