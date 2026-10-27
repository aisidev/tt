package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/capability"
	"github.com/aisidev/tt/internal/guestconfig"
	"github.com/aisidev/tt/model"
)

func readGuestConfig(path string) (model.GuestConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open guest config: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 512*1024+1))
	if err != nil {
		return nil, fmt.Errorf("read guest config: %w", err)
	}
	if len(data) > 512*1024 {
		return nil, fmt.Errorf("guest config JSON exceeds 512 KiB")
	}
	var config model.GuestConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("guest config must be a JSON object of file names and UTF-8 strings (maximum input 512 KiB)")
	}
	return config, nil
}
func readSSHKeys(values []string) (model.List[string], error) {
	keys := model.List[string]{}
	for _, value := range values {
		if (strings.HasSuffix(value, ".pub") || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~/")) && !strings.HasPrefix(value, "ssh-") {
			path := value
			if strings.HasPrefix(path, "~/") {
				path = strings.Replace(path, "~", os.Getenv("HOME"), 1)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("cannot read SSH public key %s: %w", path, err)
			}
			if !utf8.Valid(data) {
				return nil, fmt.Errorf("cannot read SSH public key %s: invalid UTF-8", path)
			}
			keys = append(keys, strings.TrimSpace(string(data)))
		} else {
			keys = append(keys, value)
		}
	}
	return keys, nil
}
func (a *application) createEnvironment(c *Client, p parsed) error {
	engine, err := model.ParseEngine(p.value("engine"))
	if err != nil {
		return err
	}
	config := model.GuestConfig{}
	if p.has("guest-config") {
		config, err = readGuestConfig(p.value("guest-config"))
		if err != nil {
			return err
		}
	}
	if err := guestconfig.Validate(engine, config, p.has("isolated-network")); err != nil {
		return err
	}
	owner, ok := os.LookupEnv("USER")
	if !ok {
		owner = "default"
	}
	if p.has("owner") {
		owner = p.value("owner")
	}
	keys, err := readSSHKeys(p.values["ssh-key"])
	if err != nil {
		return err
	}
	ports := model.List[uint16]{}
	for _, value := range p.values["port"] {
		port, err := parsePort(value)
		if err != nil {
			return err
		}
		ports = append(ports, port)
	}
	disk := p.optional32("disk")
	if err := capability.ValidateVMOptions(engine, disk, p.has("deny-outgoing"), keys, ports); err != nil {
		return err
	}
	copies := p.number("dup")
	images := p.values["image"]
	if copies == 0 || uint64(len(images))*copies > model.MaxVMs {
		return fmt.Errorf("replicas must be positive and total VM count must not exceed %d", model.MaxVMs)
	}
	vms := model.List[api.VmSpec]{}
	for _, image := range images {
		for i := uint64(0); i < copies; i++ {
			var ssh *model.SshOptions
			if len(keys) > 0 {
				ssh = &model.SshOptions{User: p.value("ssh-user"), Sudo: p.has("ssh-sudo")}
			}
			vms = append(vms, api.VmSpec{SSH: ssh, Image: image, Engine: engine, CPU: p.optional32("cpu"), Mem: p.optional32("mem"), Disk: disk, Ports: ports, DenyOutgoing: p.has("deny-outgoing"), IsolatedNetwork: p.has("isolated-network"), GuestConfig: config})
		}
	}
	var lifetime *uint64
	if p.has("lifetime") {
		value := p.number("lifetime")
		lifetime = &value
	}
	request := api.CreateEnvReq{ID: p.argument, Owner: owner, VMs: vms, Lifetime: lifetime, SSHKeys: keys}
	deadline := time.Now().Add(600 * time.Second)
	detail, err := fetch[api.EnvDetail](a.ctx, c, "POST", "/api/envs", request)
	if err != nil {
		return err
	}
	a.diagnostic("Creating environment %s; you can inspect it with 'tt env show %s'.", p.argument, p.argument)
	a.diagnostic("Expiry: %s", expiryLabel(detail.Env.ExpiresAt, a.now()))
	for detail.Env.State == model.EnvCreating {
		if !time.Now().Before(deadline) {
			return fmt.Errorf("creation is still in progress; inspect it with 'tt env show %s'", p.argument)
		}
		if err := a.sleep(a.ctx, 2*time.Second); err != nil {
			return err
		}
		detail, err = fetch[api.EnvDetail](a.ctx, c, "GET", "/api/envs/"+p.argument, nil)
		if err != nil {
			return err
		}
	}
	if detail.Env.State != model.EnvActive {
		return fmt.Errorf("environment %s is %s; resources retained. %s. Inspect with 'tt env show %s' or clean up with 'tt env delete %s'", p.argument, debugName(string(detail.Env.State)), strings.Join(detail.Warnings, "; "), p.argument, p.argument)
	}
	a.out("Environment created: %s", p.argument)
	a.out("  VMs: %d", len(detail.VMs))
	for _, vm := range detail.VMs {
		a.out("    %s [%s] %s — %s  ports: %s", vm.ID, vm.Engine, vm.Image, vm.IP, debugPorts(vm.PortMap))
	}
	a.printAccess(c, detail.VMs)
	for _, warning := range detail.Warnings {
		a.diagnostic("  warning: %s", warning)
	}
	return nil
}

func parsePort(value string) (uint16, error) {
	n, err := strconv.ParseUint(strings.TrimPrefix(value, "+"), 10, 16)
	return uint16(n), err
}
