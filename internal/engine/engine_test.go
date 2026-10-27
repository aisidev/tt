package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aisidev/tt/model"
)

func testVM() *model.Vm {
	return &model.Vm{ID: "guest", Engine: model.Qemu, CPU: 2, Mem: 256, IP: "10.10.1.2", Image: "alpine", PortMap: model.Map[uint16, uint16]{80: 20080, 22: 20022}}
}
func put(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func fakeTool(t *testing.T, dir, name, script string) {
	t.Helper()
	path := filepath.Join(dir, name)
	put(t, path, "#!/bin/sh\nset -eu\n"+script)
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
}
func TestHostEngineFactoryRejectsOtherPlatformsWithoutProbingThem(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	r := DockerRuntime
	for _, kind := range []model.Engine{model.Qemu, model.Firecracker, model.Docker, model.Bhyve, model.Jail} {
		expected := (runtime.GOOS == "linux" && (kind == model.Qemu || kind == model.Firecracker || kind == model.Docker)) || (runtime.GOOS == "freebsd" && (kind == model.Bhyve || kind == model.Jail))
		instance, err := New(kind, &r)
		if (err == nil) != expected {
			t.Fatalf("%s: %v", kind, err)
		}
		if err == nil && instance.Name() != string(kind) {
			t.Fatal(instance.Name())
		}
	}
}
func TestStartupWaitsForReadinessWithoutTreatingMissingMarkerAsExit(t *testing.T) {
	child, err := startChild(exec.Command("sleep", "5"))
	if err != nil {
		t.Fatal(err)
	}
	defer child.kill()
	probes := 0
	if err := waitForBoot(context.Background(), child, time.Second, func() bool { probes++; return probes >= 2 }); err != nil {
		t.Fatal(err)
	}
	if exited, _ := child.exited(); exited || probes != 2 {
		t.Fatalf("exited=%v probes=%d", exited, probes)
	}
}
func TestStartupReportsActualExitBeforeAcceptingReadiness(t *testing.T) {
	child, err := startChild(exec.Command("sh", "-c", "exit 7"))
	if err != nil {
		t.Fatal(err)
	}
	<-child.done
	err = waitForBoot(context.Background(), child, time.Second, func() bool { return true })
	if err == nil || !strings.Contains(err.Error(), "exited during boot") {
		t.Fatal(err)
	}
}
func TestStartupTimeoutRetainsALiveChildForInspection(t *testing.T) {
	child, err := startChild(exec.Command("sleep", "5"))
	if err != nil {
		t.Fatal(err)
	}
	defer child.kill()
	err = waitForBoot(context.Background(), child, 30*time.Millisecond, func() bool { return false })
	if err == nil || !strings.Contains(err.Error(), "resources retained") {
		t.Fatal(err)
	}
	if exited, _ := child.exited(); exited {
		t.Fatal("timeout killed child")
	}
}
func TestPrivateWriteAndRuntimeRemoval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")
	put(t, path, "old")
	if err := privateWrite(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if string(data) != "new" || info.Mode().Perm() != 0600 {
		t.Fatal(string(data), info.Mode())
	}
	if err := removeFile(dir); err == nil {
		t.Fatal("must not remove directories")
	}
	if err := removeFile(path); err != nil {
		t.Fatal(err)
	}
	if err := removeFile(path); err != nil {
		t.Fatal(err)
	}
}
func TestParentPathPreservesRelativeAndDotComponents(t *testing.T) {
	for _, tc := range []struct {
		path, parent string
		ok           bool
	}{{"", "", false}, {"/", "", false}, {"root", "", true}, {".", "", true}, {"foo/.", "", true}, {"foo/..", "foo", true}, {"/foo/./root/", "/foo", true}, {"./foo", ".", true}} {
		got, ok := parentPath(tc.path)
		if got != tc.parent || ok != tc.ok {
			t.Errorf("%q: %q,%v", tc.path, got, ok)
		}
	}
}
