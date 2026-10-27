package agent

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aisidev/tt/internal/capability"
	"github.com/aisidev/tt/internal/engine"
	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
)

type hostProbe struct {
	device   func(string) bool
	readFile func(string) ([]byte, error)
	output   func(context.Context, *exec.Cmd, time.Duration) (runcmd.Result, error)
}

func nativeProbe() hostProbe {
	return hostProbe{device: func(path string) bool {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return false
		}
		f.Close()
		return true
	}, readFile: os.ReadFile, output: runcmd.Output}
}
func (p hostProbe) probe(ctx context.Context, name string, args ...string) bool {
	out, err := p.output(ctx, exec.Command(name, args...), 5*time.Second)
	return err == nil && out.Success()
}
func (p hostProbe) engines(ctx context.Context, container *engine.ContainerRuntime) []model.Engine {
	result := []model.Engine{}
	switch runtime.GOOS {
	case "linux":
		kvm := p.device("/dev/kvm")
		if kvm && p.probe(ctx, "qemu-system-x86_64", "--version") && p.probe(ctx, "qemu-img", "--version") {
			result = append(result, model.Qemu)
		}
		if kvm && p.probe(ctx, "firecracker", "--version") && p.probe(ctx, "jailer", "--version") {
			result = append(result, model.Firecracker)
		}
	case "freebsd":
		if p.device("/dev/vmmctl") && p.probe(ctx, "which", "bhyve") && p.probe(ctx, "which", "bhyveload") && p.probe(ctx, "which", "bhyvectl") {
			result = append(result, model.Bhyve)
		}
		sysctl := func(key string) (uint32, bool) {
			out, err := p.output(ctx, exec.Command("sysctl", "-n", key), runcmd.DefaultTimeout)
			if err != nil || !out.Success() {
				return 0, false
			}
			n, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(string(out.Stdout)), "+"), 10, 32)
			return uint32(n), err == nil
		}
		jailed, ok := sysctl("security.jail.jailed")
		canCreate := ok && jailed == 0
		if !canCreate {
			children, ok := sysctl("security.jail.children.max")
			canCreate = ok && children > 0
		}
		if canCreate && p.probe(ctx, "which", "jail") && p.probe(ctx, "which", "jls") {
			result = append(result, model.Jail)
		}
	}
	if runtime.GOOS == "linux" && container != nil && p.probe(ctx, container.Command(), "--version") {
		result = append(result, model.Docker)
	}
	return result
}
func (p hostProbe) capabilities(ctx context.Context, engines []model.Engine, storage model.Storage) []string {
	caps := []string{}
	has := func(e model.Engine) bool { return slices.Contains(engines, e) }
	if runtime.GOOS == "freebsd" && has(model.Bhyve) {
		caps = append(caps, capability.LegacyBhyveEgress)
	}
	if runtime.GOOS != "linux" {
		return caps
	}
	if has(model.Qemu) || has(model.Firecracker) {
		// keygen/keyscan print help with a failing exit code. Executability, not
		// success status, is the Rust capability probe here.
		_, keygen := p.output(ctx, exec.Command("ssh-keygen", "-?"), 5*time.Second)
		if keygen == nil {
			_, keyscan := p.output(ctx, exec.Command("ssh-keyscan", "-?"), 5*time.Second)
			if keyscan == nil {
				caps = append(caps, capability.LegacySSH)
			}
		}
	}
	if has(model.Qemu) && (storage == model.File || p.probe(ctx, "zfs", "version")) {
		caps = append(caps, capability.LegacyQemuResources)
	}
	if p.probe(ctx, "nft", "--version") && p.probe(ctx, "ip", "-Version") {
		caps = append(caps, capability.LegacyIsolation)
	}
	if has(model.Firecracker) {
		controllers, _ := p.readFile("/sys/fs/cgroup/cgroup.controllers")
		if !utf8.Valid(controllers) {
			controllers = nil
		}
		fields := strings.Fields(string(controllers))
		if slices.Contains(fields, "cpu") && slices.Contains(fields, "memory") && slices.Contains(fields, "pids") {
			caps = append(caps, capability.LegacyFCJailer)
		}
		if p.probe(ctx, "mkfs.ext4", "-V") {
			caps = append(caps, capability.LegacyConfig)
		}
		if p.probe(ctx, "e2fsck", "-V") && p.probe(ctx, "which", "resize2fs") {
			caps = append(caps, capability.LegacyFCDisk, capability.LegacyFCResources)
		}
		if storage == model.Zvol && p.probe(ctx, "zfs", "version") {
			caps = append(caps, capability.LegacyFCZvol)
		}
	}
	return caps
}
