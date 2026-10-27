package engine

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
)

type Docker struct{ runtime ContainerRuntime }

func (d *Docker) Name() string          { return "docker" }
func containerName(vm *model.Vm) string { return "tt-" + vm.ID }
func (d *Docker) inspect(ctx context.Context, vm *model.Vm) (*model.VmState, error) {
	out, err := runcmd.Output(ctx, exec.Command(d.runtime.Command(), "inspect", "-f", "{{.State.Status}}", containerName(vm)), 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("inspect container: %w", err)
	}
	if !out.Success() {
		message := string(out.Stderr)
		lower := strings.ToLower(message)
		if strings.Contains(lower, "no such object") || strings.Contains(lower, "no such container") || strings.Contains(lower, "no container with name or id") {
			return nil, nil
		}
		return nil, fmt.Errorf("container inspect failed: %s", message)
	}
	state := model.VmFailed
	switch strings.TrimSpace(string(out.Stdout)) {
	case "running":
		state = model.VmRunning
	case "paused":
		state = model.VmPaused
	case "exited", "created":
		state = model.VmStopped
	}
	return &state, nil
}
func (d *Docker) command(vm *model.Vm) *exec.Cmd {
	args := []string{"run", "-d", "--name", containerName(vm), "--cpus", strconv.FormatUint(uint64(vm.CPU), 10), "--memory", fmt.Sprintf("%dm", vm.Mem), "--memory-swap", fmt.Sprintf("%dm", vm.Mem)}
	ports := make([]uint16, 0, len(vm.PortMap))
	for guest := range vm.PortMap {
		ports = append(ports, guest)
	}
	slices.Sort(ports)
	for _, guest := range ports {
		args = append(args, "-p", fmt.Sprintf("%d:%d", vm.PortMap[guest], guest))
	}
	args = append(args, vm.Image)
	return exec.Command(d.runtime.Command(), args...)
}
func (d *Docker) Create(ctx context.Context, vm *model.Vm, image, format string, keys []string) error {
	out, err := runcmd.Output(ctx, d.command(vm), runcmd.DefaultTimeout)
	if err != nil || !out.Success() {
		// A timeout or a lost response does not prove the daemon did not
		// create the container; a blind retry would die on the name. Adopt the
		// leftover only when it provably carries this VM's image, otherwise
		// surface the original failure untouched.
		if !d.adoptable(ctx, vm) {
			if err != nil {
				return fmt.Errorf("failed to spawn container: %w", err)
			}
			return fmt.Errorf("%s run failed: %s", d.runtime.Command(), out.Stderr)
		}
	}
	state, err := d.State(ctx, vm)
	if err != nil {
		return err
	}
	if state != model.VmRunning {
		return fmt.Errorf("container exited during creation; use an image with a long-running command")
	}
	return nil
}

// adoptable reports whether a container already carrying this VM's name also
// carries its image. Container names are tt-<vm id> and the spec is immutable
// after planning, so an image match identifies the earlier, lost creation.
func (d *Docker) adoptable(ctx context.Context, vm *model.Vm) bool {
	if ctx.Err() != nil {
		// The failed run may have consumed the caller's deadline; the probe
		// stays bounded either way.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	for range 5 {
		out, err := runcmd.Output(ctx, exec.Command(d.runtime.Command(), "inspect", "-f", "{{.Config.Image}}", containerName(vm)), 2*time.Second)
		if err == nil && out.Success() {
			return strings.TrimSpace(string(out.Stdout)) == vm.Image
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(300 * time.Millisecond):
		}
	}
	return false
}
func (d *Docker) Start(ctx context.Context, vm *model.Vm) error {
	state, err := d.State(ctx, vm)
	if err != nil {
		return err
	}
	action := "start"
	if state == model.VmPaused {
		action = "unpause"
	}
	out, err := runcmd.Run(ctx, d.runtime.Command(), action, containerName(vm))
	if err != nil {
		return err
	}
	if !out.Success() {
		return fmt.Errorf("container %s failed: %s", action, out.Stderr)
	}
	state, err = d.State(ctx, vm)
	if err != nil {
		return err
	}
	if state != model.VmRunning {
		return fmt.Errorf("container exited during startup; check %s logs %s", d.runtime.Command(), containerName(vm))
	}
	return nil
}
func (d *Docker) Stop(ctx context.Context, vm *model.Vm) error {
	state, err := d.inspect(ctx, vm)
	if err != nil {
		return err
	}
	if state == nil {
		return nil
	}
	if *state == model.VmPaused {
		// Older daemons cannot stop a paused container; unpause first.
		out, err := runcmd.Run(ctx, d.runtime.Command(), "unpause", containerName(vm))
		if err != nil {
			return err
		}
		if !out.Success() {
			return fmt.Errorf("container unpause failed: %s", out.Stderr)
		}
	}
	out, err := runcmd.Run(ctx, d.runtime.Command(), "stop", "-t", "10", containerName(vm))
	if err != nil {
		return err
	}
	if !out.Success() {
		state, err := d.inspect(ctx, vm)
		if err != nil {
			return err
		}
		if state == nil {
			return nil
		}
		return fmt.Errorf("container stop failed: %s", out.Stderr)
	}
	return nil
}
func (d *Docker) Destroy(ctx context.Context, vm *model.Vm) error {
	state, err := d.inspect(ctx, vm)
	if err != nil {
		return err
	}
	if state == nil {
		return nil
	}
	out, err := runcmd.Run(ctx, d.runtime.Command(), "rm", "-f", containerName(vm))
	if err != nil {
		return err
	}
	if !out.Success() {
		return fmt.Errorf("container remove failed")
	}
	return nil
}
func (d *Docker) State(ctx context.Context, vm *model.Vm) (model.VmState, error) {
	state, err := d.inspect(ctx, vm)
	if err != nil {
		return "", err
	}
	if state == nil {
		return model.VmFailed, nil
	}
	return *state, nil
}
