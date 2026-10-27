//go:build linux

package engine

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/model"
)

// SetStopGrace(0) must reach newly constructed engines: zero means no
// graceful wait at all, the engine-specific default must survive a -1,
// and out-of-range values clamp to the default.
func TestStopGracePlumbing(t *testing.T) {
	defer SetStopGrace(-1)

	if got := newFirecracker().stopGrace; got != fcDefaultStopGrace {
		t.Fatalf("default grace: got %v", got)
	}
	SetStopGrace(0)
	if got := newFirecracker().stopGrace; got != 0 {
		t.Fatalf("grace 0: got %v", got)
	}
	if got := newQemu().stopGrace; got != 0 {
		t.Fatalf("grace 0 qemu: got %v", got)
	}
	SetStopGrace(7)
	if got := newQemu().stopGrace; got != 7*time.Second {
		t.Fatalf("grace 7 qemu: got %v", got)
	}
	SetStopGrace(-42)
	if got := newFirecracker().stopGrace; got != fcDefaultStopGrace {
		t.Fatalf("clamped grace: got %v", got)
	}
}

func TestPerCallStopPolicyIsIsolatedAndGracefulOnlyRetainsGuest(t *testing.T) {
	seconds := uint32(2)
	original := newQemu()
	original.stopGrace = 0
	configured, err := ApplyStopPolicy(original, model.StopOptions{GraceSeconds: &seconds, GracefulOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if configured.(*Qemu).stopGrace != 2*time.Second || !configured.(*Qemu).gracefulOnly || original.stopGrace != 0 || original.gracefulOnly {
		t.Fatal("per-call policy mutated a shared engine")
	}
	for _, options := range []model.StopOptions{{Force: true, GracefulOnly: true}, {GraceSeconds: new(uint32)}} {
		if _, err := ApplyStopPolicy(original, options); err == nil {
			t.Fatal("invalid policy admitted")
		}
	}
	child := exec.Command("/bin/sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	original.runDir = t.TempDir()
	original.processes = processes{root: t.TempDir()}
	vm := testVM()
	pid := uint32(child.Process.Pid)
	fakeProcess(t, original.processes, pid, "qemu\x00"+original.pidPath(vm)+"\x00")
	put(t, original.pidPath(vm), strconv.Itoa(child.Process.Pid))
	safe, err := ApplyStopPolicy(original, model.StopOptions{GracefulOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var typed *api.BoundaryError
	if err = safe.Stop(ctx, vm); !errors.As(err, &typed) || typed.Code != "graceful_timeout" {
		t.Fatal("graceful-only stop did not retain guest", err)
	}
	if err = child.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("guest was killed", err)
	}
}
