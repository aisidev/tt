// Package engine implements host-gated workload lifecycles.
package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
	"golang.org/x/sys/unix"
)

type Engine interface {
	Create(context.Context, *model.Vm, string, string, []string) error
	Start(context.Context, *model.Vm) error
	Stop(context.Context, *model.Vm) error
	Destroy(context.Context, *model.Vm) error
	State(context.Context, *model.Vm) (model.VmState, error)
	Name() string
}

// StateObserver supplies bounded evidence without repairing runtime files,
// starting workloads, changing networking or replaying persistent intent.
type StateObserver interface {
	ObserveState(context.Context, *model.Vm) (model.VmState, error)
}

type ContainerRuntime string

const (
	DockerRuntime ContainerRuntime = "docker"
	PodmanRuntime ContainerRuntime = "podman"
)

func ParseContainerRuntime(value string) (ContainerRuntime, error) {
	if value != "docker" && value != "podman" {
		return "", fmt.Errorf("container runtime must be docker or podman")
	}
	return ContainerRuntime(value), nil
}
func (r ContainerRuntime) Command() string { return string(r) }
func (r ContainerRuntime) Available(ctx context.Context) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	out, err := runcmd.Output(ctx, exec.Command(r.Command(), "--version"), 5*time.Second)
	return err == nil && out.Success()
}
func New(kind model.Engine, containerRuntime *ContainerRuntime) (Engine, error) {
	return platformEngine(kind, containerRuntime)
}

func privateWrite(path string, data []byte) error {
	parent := filepath.Dir(path)
	f, err := os.CreateTemp(parent, ".tt-private-*")
	if err != nil {
		return fmt.Errorf("private staging file: %w", err)
	}
	defer f.Close()
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write private file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync private file: %w", err)
	}
	return os.Rename(f.Name(), path)
}
func removeFile(path string) error {
	err := unix.Unlink(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove runtime file: %w", err)
	}
	return nil
}
func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}
func executable(name string) (string, error) {
	for _, dir := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return canonicalPath(path)
		}
	}
	return "", fmt.Errorf("Firecracker requires matching firecracker and jailer binaries in PATH")
}
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// parentPath preserves an empty relative parent. filepath.Dir("root") == "."
// must never turn malformed metadata into deletion of the current directory.
func parentPath(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	absolute := strings.HasPrefix(path, "/")
	components := []string{}
	for index, component := range strings.Split(path, "/") {
		if component == "" || (component == "." && (absolute || index > 0)) {
			continue
		}
		components = append(components, component)
	}
	if len(components) == 0 {
		return "", false
	}
	parent := strings.Join(components[:len(components)-1], "/")
	if absolute {
		parent = "/" + parent
	}
	return parent, true
}

type ownedChild struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func startChild(cmd *exec.Cmd) (*ownedChild, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	child := &ownedChild{cmd: cmd, done: make(chan struct{})}
	go func() { child.err = cmd.Wait(); close(child.done) }()
	return child, nil
}
func (c *ownedChild) exited() (bool, error) {
	select {
	case <-c.done:
		return true, c.err
	default:
		return false, nil
	}
}
func (c *ownedChild) kill() { _ = c.cmd.Process.Kill(); <-c.done }
func waitForBoot(ctx context.Context, child *ownedChild, timeout time.Duration, ready func() bool) error {
	started := time.Now()
	for time.Since(started) < timeout {
		if exited, _ := child.exited(); exited {
			return fmt.Errorf("Firecracker jailer exited during boot; inspect the jailer/VMM log")
		}
		if ready() {
			return nil
		}
		if err := sleepContext(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("Firecracker did not become ready; resources retained for inspection")
}
