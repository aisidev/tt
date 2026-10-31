package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/guestconfig"
	"github.com/aisidev/tt/model"
)

func seededStoppedVM(t *testing.T) (*Runtime, *fakeEngine, *model.Vm) {
	t.Helper()
	r, engine, _ := runtimeFixture(t)
	files := model.GuestConfig{"om-guest.json": "original-private-bootstrap"}
	vm := &model.Vm{ID: "seeded", EnvID: "env-g1", HostID: "host", Engine: model.Firecracker, Image: "base", State: model.VmStopped, CPU: 1, Mem: 128, Disk: 132, IP: "10.10.0.2", Options: model.VmOptions{RequestedDisk: 128, GuestConfigDigest: guestconfig.Digest(files)}}
	if err := saveVM(context.Background(), r.db.conn, vm); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.clonePath(vm), 0700); err != nil {
		t.Fatal(err)
	}
	return r, engine, vm
}
func TestGuestConfigStartRequiresPhysicalStopBeforeAnyPublication(t *testing.T) {
	r, engine, vm := seededStoppedVM(t)
	engine.state = model.VmRunning
	err := r.startVM(context.Background(), vm.ID, model.GuestConfig{"om-guest.json": "replacement"})
	var typed *api.BoundaryError
	if !errors.As(err, &typed) || typed.Code != "state_conflict" || engine.creates != 0 {
		t.Fatalf("physical conflict not refused: %v", err)
	}
	if _, err = os.Stat(filepath.Join(r.clonePath(vm), guestconfig.ConfigDisk)); !os.IsNotExist(err) {
		t.Fatal("configuration written for live process")
	}
}
func TestGuestConfigStartFailedBuildRetainsPreviousDiskAndStopsLaunch(t *testing.T) {
	r, engine, vm := seededStoppedVM(t)
	target := filepath.Join(r.clonePath(vm), guestconfig.ConfigDisk)
	if err := os.WriteFile(target, []byte("original-disk"), 0600); err != nil {
		t.Fatal(err)
	}
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "mkfs.ext4"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools)
	if err := r.startVM(context.Background(), vm.ID, model.GuestConfig{"om-guest.json": "replacement"}); err == nil {
		t.Fatal("failed seed accepted")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "original-disk" || engine.creates != 0 {
		t.Fatal("old disk changed or guest launched")
	}
	retained, err := r.requireVM(context.Background(), vm.ID)
	if err != nil || retained.State != model.VmStopped {
		t.Fatal("failed seed invented a launch")
	}
}
func TestGuestConfigStartPublishesRealDiskBeforeEngineAndPreservesReplay(t *testing.T) {
	if os.Getenv("TT_TEST_INTEGRATION") != "1" {
		t.Skip("requires authorized real ext4 tools")
	}
	for _, tool := range []string{"mkfs.ext4", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatal(err)
		}
	}
	r, engine, vm := seededStoppedVM(t)
	replacement := model.GuestConfig{"om-guest.json": "new-private-bootstrap"}
	engine.beforeCreate = func(current *model.Vm) {
		data, err := exec.Command("debugfs", "-R", "cat om-guest.json", filepath.Join(r.clonePath(vm), guestconfig.ConfigDisk)).Output()
		if err != nil || string(data) != replacement["om-guest.json"] {
			t.Error("engine reached before exact bootstrap publication")
		}
		if current.Options.GuestConfigDigest == nil || *current.Options.GuestConfigDigest != *guestconfig.Digest(replacement) {
			t.Error("seed digest not committed before launch")
		}
	}
	if err := r.startVM(context.Background(), vm.ID, replacement); err != nil {
		t.Fatal(err)
	}
	if err := r.startVM(context.Background(), vm.ID, replacement); err != nil || engine.creates != 1 {
		t.Fatal("original replay relaunched guest", err)
	}
	if err := r.startVM(context.Background(), vm.ID, model.GuestConfig{"om-guest.json": "different"}); err == nil || engine.creates != 1 {
		t.Fatal("running guest reseeded")
	}
}
