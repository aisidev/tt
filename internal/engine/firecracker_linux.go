package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/aisidev/tt/internal/netcfg"
	"github.com/aisidev/tt/model"
)

type Firecracker struct {
	controlParent      string
	gracefulOnly       bool
	runDir, cgroupRoot string
	processes          processes
	chown              func(string, int, int) error
	prepareTap         func(context.Context, string, uint32) error
	// stopGrace bounds the graceful CtrlAltDel wait; 0 = force kill
	// immediately. Defaults to 30 s (stopGraceSeconds override applies).
	stopGrace time.Duration
}

const fcDefaultStopGrace = 30 * time.Second

func newFirecracker() *Firecracker {
	grace := fcDefaultStopGrace
	if stopGraceSeconds >= 0 {
		grace = time.Duration(stopGraceSeconds) * time.Second
	}
	return &Firecracker{runDir: model.RunDir, cgroupRoot: "/sys/fs/cgroup", processes: hostProcesses(), chown: os.Chown, prepareTap: netcfg.New().PrepareJailedTap, stopGrace: grace}
}
func (*Firecracker) Name() string { return "firecracker" }
func firecrackerConfiguration(vm *model.Vm, mac string) map[string]any {
	drives := []map[string]any{{"drive_id": "rootfs", "path_on_host": "/rootfs.ext4", "is_root_device": true, "is_read_only": false}}
	if vm.Options.ConfigDiskMiB(vm.Engine) > 0 {
		drives = append(drives, map[string]any{"drive_id": "config", "path_on_host": "/guest-config.ext4", "is_root_device": false, "is_read_only": true})
	}
	for index := range vm.Options.DataVolumes {
		drives = append(drives, map[string]any{"drive_id": fmt.Sprintf("data-%d", index), "path_on_host": fmt.Sprintf("/data-%d.raw", index), "is_root_device": false, "is_read_only": false})
	}
	return map[string]any{
		"boot-source":        map[string]any{"kernel_image_path": "/vmlinux", "boot_args": "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw init=/sbin/init ip=" + vm.IP + "::10.10.0.1:255.255.0.0::eth0:off"},
		"drives":             drives,
		"entropy":            map[string]any{"rate_limiter": map[string]any{"bandwidth": map[string]any{"size": 4096, "refill_time": 100}}},
		"machine-config":     map[string]any{"vcpu_count": vm.CPU, "mem_size_mib": vm.Mem},
		"network-interfaces": []map[string]any{{"iface_id": "eth0", "host_dev_name": netcfg.TapName(vm.ID), "guest_mac": mac}},
	}
}
func (f *Firecracker) pidPath(vm *model.Vm) string {
	return filepath.Join(f.runDir, "fc-"+vm.ID+".pid")
}
func (f *Firecracker) legacySocket(vm *model.Vm) string {
	return filepath.Join(f.runDir, "fc-"+vm.ID+".sock")
}
func (f *Firecracker) readPID(vm *model.Vm) (uint32, error) {
	markers := []string{"/fc-" + netcfg.TapName(vm.ID) + ".sock", f.legacySocket(vm)}
	s, err := f.loadSandbox(vm)
	if err != nil {
		return 0, err
	}
	if s != nil {
		markers = append(markers, s.Marker)
	}
	return f.processes.recover(f.pidPath(vm), markers)
}
func (f *Firecracker) identity(vm *model.Vm) (string, string, error) {
	s, err := f.loadSandbox(vm)
	if err != nil {
		return "", "", err
	}
	if s != nil {
		return s.socket(), s.Marker, nil
	}
	pid, err := f.readPID(vm)
	if err != nil {
		return "", "", err
	}
	if pid != 0 {
		matches, err := f.processes.matches(pid, "/fc-"+netcfg.TapName(vm.ID)+".sock")
		if err != nil {
			return "", "", err
		}
		if matches {
			return "", "", fmt.Errorf("Firecracker sandbox metadata is missing; restore it before lifecycle operations")
		}
	}
	return f.legacySocket(vm), f.legacySocket(vm), nil
}
func firecrackerRequest(ctx context.Context, socket, method, path string, body *string) ([]byte, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(*body)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("Firecracker API %s %s failed: %w", method, path, err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return nil, fmt.Errorf("Firecracker API %s %s failed: HTTP %d", method, path, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("Firecracker API %s %s failed: %w", method, path, err)
	}
	return data, nil
}
func (f *Firecracker) PrepareDiskRestore(ctx context.Context, vm *model.Vm) error {
	pid, err := f.readPID(vm)
	if err != nil {
		return err
	}
	if pid != 0 {
		_, marker, err := f.identity(vm)
		if err != nil {
			return err
		}
		match, err := f.processes.matches(pid, marker)
		if err != nil {
			return err
		}
		if match {
			return fmt.Errorf("Firecracker is still running")
		}
	}
	s, err := f.loadSandbox(vm)
	if err != nil {
		return err
	}
	if s != nil {
		return s.cleanup()
	}
	return nil
}
func (f *Firecracker) Create(ctx context.Context, vm *model.Vm, image, format string, keys []string) error {
	if err := os.MkdirAll(f.runDir, 0777); err != nil {
		return fmt.Errorf("create runtime dir: %w", err)
	}
	pid, err := f.readPID(vm)
	if err != nil {
		return err
	}
	if pid != 0 {
		_, marker, err := f.identity(vm)
		if err != nil {
			return err
		}
		match, err := f.processes.matches(pid, marker)
		if err != nil {
			return err
		}
		if match {
			return fmt.Errorf("Firecracker is still running; refusing duplicate launch")
		}
	}
	old, err := f.loadSandbox(vm)
	if err != nil {
		return err
	}
	if old != nil {
		if err := old.cleanup(); err != nil {
			return err
		}
	}
	s, cmd, err := f.prepareSandbox(ctx, vm, image)
	if err != nil {
		return err
	}
	logPath := filepath.Join(f.runDir, "fc-"+vm.ID+".log")
	if err := privateWrite(logPath, nil); err != nil {
		return err
	}
	log, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("console log: %w", err)
	}
	defer log.Close()
	cmd.Stdout = log
	cmd.Stderr = log
	child, err := startChild(cmd)
	if err != nil {
		return fmt.Errorf("spawn Firecracker jailer: %w", err)
	}
	if err := privateWrite(f.pidPath(vm), []byte(strconv.Itoa(cmd.Process.Pid))); err != nil {
		child.kill()
		return err
	}
	return waitForBoot(ctx, child, 10*time.Second, func() bool {
		if !exists(s.socket()) {
			return false
		}
		state, err := f.State(ctx, vm)
		return err == nil && state == model.VmRunning
	})
}
func (f *Firecracker) Start(ctx context.Context, vm *model.Vm) error {
	socket, _, err := f.identity(vm)
	if err != nil {
		return err
	}
	body := `{"state":"Resumed"}`
	_, err = firecrackerRequest(ctx, socket, "PATCH", "/vm", &body)
	return err
}
func (f *Firecracker) Stop(ctx context.Context, vm *model.Vm) error {
	pid, err := f.readPID(vm)
	if err != nil || pid == 0 {
		return err
	}
	socket, marker, err := f.identity(vm)
	if err != nil {
		return err
	}
	match, err := f.processes.matches(pid, marker)
	if err != nil || !match {
		return err
	}
	if state, err := f.State(ctx, vm); err == nil && state == model.VmPaused && f.stopGrace > 0 {
		if err := f.Start(ctx, vm); err != nil {
			if f.gracefulOnly {
				return gracefulTimeout()
			}
			return f.processes.terminate(ctx, pid, marker)
		}
	}
	if runtime.GOARCH == "amd64" && f.stopGrace > 0 {
		body := `{"action_type":"SendCtrlAltDel"}`
		if _, err := firecrackerRequest(ctx, socket, "PUT", "/actions", &body); err == nil {
			deadline := time.Now().Add(f.stopGrace)
			for time.Now().Before(deadline) {
				match, err := f.processes.matches(pid, marker)
				if err != nil || !match {
					return err
				}
				if err := sleepContext(ctx, 100*time.Millisecond); err != nil {
					return err
				}
			}
		}
	}
	if f.gracefulOnly {
		return gracefulTimeout()
	}
	fmt.Fprintf(os.Stderr, "[firecracker] %s: graceful shutdown unavailable or timed out; forcing stop\n", vm.ID)
	return f.processes.terminate(ctx, pid, marker)
}
func (f *Firecracker) Destroy(ctx context.Context, vm *model.Vm) error {
	if err := f.Stop(ctx, vm); err != nil {
		return err
	}
	s, err := f.loadSandbox(vm)
	if err != nil {
		return err
	}
	if s != nil {
		if err := s.cleanup(); err != nil {
			return err
		}
	}
	for _, path := range []string{f.sandboxPath(vm), f.pidPath(vm), f.legacySocket(vm), filepath.Join(f.runDir, "fc-"+vm.ID+".json"), filepath.Join(f.runDir, "fc-"+vm.ID+".log")} {
		if err := removeFile(path); err != nil {
			return err
		}
	}
	return nil
}
func (f *Firecracker) State(ctx context.Context, vm *model.Vm) (model.VmState, error) {
	pid, err := f.readPID(vm)
	if err != nil {
		return "", err
	}
	if pid == 0 {
		return model.VmStopped, nil
	}
	socket, marker, err := f.identity(vm)
	if err != nil {
		return "", err
	}
	match, err := f.processes.matches(pid, marker)
	if err != nil {
		return "", err
	}
	if !match {
		return model.VmStopped, nil
	}
	data, err := firecrackerRequest(ctx, socket, "GET", "/", nil)
	if err != nil {
		return "", err
	}
	var body any
	if err := json.Unmarshal(data, &body); err != nil {
		return "", fmt.Errorf("Firecracker status: %w", err)
	}
	record, _ := body.(map[string]any)
	state, _ := record["state"].(string)
	switch state {
	case "Running":
		return model.VmRunning, nil
	case "Paused":
		return model.VmPaused, nil
	case "Not started":
		return model.VmCreating, nil
	default:
		return "", fmt.Errorf("unexpected Firecracker state")
	}
}

func (f *Firecracker) withStopPolicy(options model.StopOptions) Engine {
	clone := *f
	clone.stopGrace = policyGrace(f.stopGrace, options)
	clone.gracefulOnly = options.GracefulOnly
	return &clone
}
