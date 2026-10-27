package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aisidev/tt/internal/netcfg"
	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
	"golang.org/x/sys/unix"
)

type Bhyve struct {
	runDir, deviceDir string
	tapDevice         func(context.Context, string) (string, error)
	signal            func(int, unix.Signal) error
}

func newBhyve() *Bhyve {
	return &Bhyve{runDir: model.RunDir, deviceDir: "/dev/vmm", tapDevice: netcfg.New().BhyveTapDevice, signal: unix.Kill}
}
func (b *Bhyve) Name() string { return "bhyve" }
func (b *Bhyve) artifact(vm *model.Vm, suffix string) string {
	return filepath.Join(b.runDir, "bhyve-"+vm.ID+"."+suffix)
}
func (b *Bhyve) device(vm *model.Vm) string { return filepath.Join(b.deviceDir, vm.ID) }

// The FreeBSD device path must distinguish absence from an inspection failure.
// Linux's observational paths no longer use this helper.
func tryExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func processListing(ctx context.Context) (string, error) {
	out, err := runcmd.Run(ctx, "ps", "-axww", "-o", "pid=", "-o", "stat=", "-o", "comm=", "-o", "args=")
	if err != nil {
		return "", fmt.Errorf("inspect bhyve processes: %w", err)
	}
	if !out.Success() {
		return "", fmt.Errorf("cannot query bhyve processes")
	}
	if !utf8.Valid(out.Stdout) {
		return "", fmt.Errorf("invalid process inventory encoding")
	}
	return string(out.Stdout), nil
}
func findBhyveProcess(id, listing string) (int, error) {
	if strings.TrimSpace(listing) == "" {
		return 0, fmt.Errorf("empty process inventory; retain resources")
	}
	found := 0
	for _, line := range strings.Split(listing, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return 0, fmt.Errorf("invalid process inventory; retain resources")
		}
		pid, err := strconv.ParseInt(fields[0], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("invalid process inventory; retain resources")
		}
		if strings.HasPrefix(fields[1], "Z") || fields[2] != "bhyve" {
			continue
		}
		if len(fields) < 4 {
			return 0, fmt.Errorf("unreadable bhyve process identity; retain resources")
		}
		args := fields[3:]
		match := (args[0] == "bhyve:" && len(args) > 1 && args[1] == id) || (args[len(args)-1] == id && !strings.HasSuffix(args[0], ":"))
		if match {
			if found != 0 {
				return 0, fmt.Errorf("multiple bhyve processes match; retain resources")
			}
			if pid <= 0 {
				return 0, fmt.Errorf("invalid bhyve PID")
			}
			found = int(pid)
		}
	}
	return found, nil
}
func (b *Bhyve) pid(ctx context.Context, vm *model.Vm) (int, error) {
	listing, err := processListing(ctx)
	if err != nil {
		return 0, err
	}
	pid, err := findBhyveProcess(vm.ID, listing)
	if err != nil {
		return 0, err
	}
	if pid != 0 {
		info, err := os.Stat(b.artifact(vm, "disk"))
		if err != nil || !info.Mode().IsRegular() {
			return 0, fmt.Errorf("bhyve ownership metadata missing; retain resources")
		}
	}
	return pid, nil
}
func (b *Bhyve) removeDevice(ctx context.Context, vm *model.Vm) error {
	present, err := tryExists(b.device(vm))
	if err != nil {
		return fmt.Errorf("inspect bhyve device: %w", err)
	}
	if !present {
		return nil
	}
	info, err := os.Stat(b.artifact(vm, "disk"))
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("unowned bhyve device; retain resources")
	}
	out, err := runcmd.Run(ctx, "bhyvectl", "--destroy", "--vm", vm.ID)
	if err != nil {
		return fmt.Errorf("destroy bhyve device: %w", err)
	}
	if !out.Success() {
		return fmt.Errorf("bhyvectl destroy failed: %s", out.Stderr)
	}
	return nil
}
func bhyveCommand(vm *model.Vm, disk, tap string) *exec.Cmd {
	return exec.Command("bhyve", "-A", "-H", "-P", "-c", strconv.FormatUint(uint64(vm.CPU), 10), "-m", fmt.Sprintf("%dM", vm.Mem), "-s", "0:0,hostbridge", "-s", "3:0,virtio-blk,"+disk, "-s", "4:0,virtio-net,"+tap, "-s", "31,lpc", "-l", "com1,stdio", vm.ID)
}
func (b *Bhyve) Create(ctx context.Context, vm *model.Vm, image, format string, keys []string) error {
	pid, err := b.pid(ctx, vm)
	if err != nil {
		return err
	}
	if pid != 0 {
		return fmt.Errorf("bhyve is still running")
	}
	if err := b.removeDevice(ctx, vm); err != nil {
		return err
	}
	if err := os.MkdirAll(b.runDir, 0777); err != nil {
		return err
	}
	disk, err := canonicalPath(image)
	if err != nil {
		return fmt.Errorf("resolve bhyve disk: %w", err)
	}
	if err := os.WriteFile(b.artifact(vm, "disk"), []byte(disk), 0666); err != nil {
		return fmt.Errorf("record bhyve disk: %w", err)
	}
	cmd := exec.Command("bhyveload", "-m", fmt.Sprintf("%dM", vm.Mem), "-e", "autoboot_delay=0", "-d", disk, vm.ID)
	out, err := runcmd.Output(ctx, cmd, runcmd.DefaultTimeout)
	if err != nil {
		return fmt.Errorf("run bhyveload: %w", err)
	}
	if !out.Success() {
		return fmt.Errorf("bhyveload failed: %s", out.Stderr)
	}
	file, err := os.Create(b.artifact(vm, "log"))
	if err != nil {
		return fmt.Errorf("create bhyve log: %w", err)
	}
	defer file.Close()
	tap, err := b.tapDevice(ctx, vm.ID)
	if err != nil {
		return err
	}
	cmd = bhyveCommand(vm, disk, tap)
	cmd.Stdout = file
	cmd.Stderr = file
	child, err := startChild(cmd)
	if err != nil {
		return fmt.Errorf("spawn bhyve: %w", err)
	}
	if err := os.WriteFile(b.artifact(vm, "pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0666); err != nil {
		// Without the recorded identity this VM is unsupervised and its retry
		// would deadlock against itself; tear it down like Firecracker does.
		child.kill()
		return fmt.Errorf("record bhyve PID: %w", err)
	}
	for i := 0; i < 10; i++ {
		if exited, _ := child.exited(); exited {
			status := cmd.ProcessState.String()
			if cmd.ProcessState.Exited() {
				status = fmt.Sprintf("exit status: %d", cmd.ProcessState.ExitCode())
			}
			return fmt.Errorf("bhyve exited during startup (%s); inspect its runtime log", status)
		}
		if err := sleepContext(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}
func (b *Bhyve) Start(ctx context.Context, vm *model.Vm) error {
	return fmt.Errorf("stopped bhyve VMs require cold creation from their retained disk")
}
func (b *Bhyve) Stop(ctx context.Context, vm *model.Vm) error {
	for _, signal := range []unix.Signal{unix.SIGTERM, unix.SIGKILL} {
		pid, err := b.pid(ctx, vm)
		if err != nil {
			return err
		}
		if pid == 0 {
			break
		}
		if signal == unix.SIGKILL {
			fmt.Fprintf(os.Stderr, "[bhyve] %s did not stop after ACPI request; forcing termination\n", vm.ID)
		}
		if err := b.signal(pid, signal); err != nil && !errors.Is(err, unix.ESRCH) {
			return err
		}
		for i := 0; i < 100; i++ {
			pid, err := b.pid(ctx, vm)
			if err != nil {
				return err
			}
			if pid == 0 {
				break
			}
			if err := sleepContext(ctx, 100*time.Millisecond); err != nil {
				return err
			}
		}
	}
	pid, err := b.pid(ctx, vm)
	if err != nil {
		return err
	}
	if pid != 0 {
		return fmt.Errorf("bhyve process did not exit; retain disk")
	}
	return b.removeDevice(ctx, vm)
}
func (b *Bhyve) Destroy(ctx context.Context, vm *model.Vm) error {
	if err := b.Stop(ctx, vm); err != nil {
		return err
	}
	for _, suffix := range []string{"pid", "disk", "log"} {
		if err := removeFile(b.artifact(vm, suffix)); err != nil {
			return err
		}
	}
	return nil
}
func (b *Bhyve) State(ctx context.Context, vm *model.Vm) (model.VmState, error) {
	pid, err := b.pid(ctx, vm)
	if err != nil {
		return "", err
	}
	if pid == 0 {
		return model.VmStopped, nil
	}
	return model.VmRunning, nil
}
