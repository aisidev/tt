package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aisidev/tt/model"
)

func TestBoundRuntimeNeverDeletesFromAnotherContainerStore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TT_ENGINE_TEST_DIR", dir)
	t.Setenv("PATH", dir)
	for _, runtime := range []string{"docker", "podman"} {
		put(t, filepath.Join(dir, runtime+"-owned"), "running")
		fakeTool(t, dir, runtime, fmt.Sprintf(`case "$1" in
 --version) exit 0 ;;
 inspect) if [ -e "$TT_ENGINE_TEST_DIR/%s-owned" ]; then echo running; else echo 'No such container' >&2; exit 1; fi ;;
 stop) exit 0 ;;
 rm) /bin/rm "$TT_ENGINE_TEST_DIR/%s-owned" ;;
 *) exit 2 ;;
esac
`, runtime, runtime))
	}
	ctx := context.Background()
	vm := testVM()
	d := &Docker{runtime: PodmanRuntime}
	if !DockerRuntime.Available(ctx) {
		t.Fatal("fake docker unavailable")
	}
	if err := d.Stop(ctx, vm); err != nil {
		t.Fatal(err)
	}
	if err := d.Destroy(ctx, vm); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dir, "podman-owned")) || !exists(filepath.Join(dir, "docker-owned")) {
		t.Fatal("wrong store modified")
	}
	if err := os.Remove(filepath.Join(dir, "podman")); err != nil {
		t.Fatal(err)
	}
	if d.Stop(ctx, vm) == nil || d.Destroy(ctx, vm) == nil {
		t.Fatal("silently switched runtime")
	}
	if !exists(filepath.Join(dir, "docker-owned")) {
		t.Fatal("docker store changed")
	}
}
func TestMissingContainerCleanupDoesNotHideDaemonOrStopErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	fakeTool(t, dir, "docker", `for arg; do name="$arg"; done
case "$1:$name" in
 inspect:tt-missing) echo 'No such container: tt-missing' >&2; exit 1 ;;
 inspect:tt-unavailable) echo 'Cannot connect to the Docker daemon' >&2; exit 1 ;;
 inspect:tt-running) echo running ;;
 stop:tt-running) echo 'permission denied' >&2; exit 1 ;;
 *) echo 'unexpected mutating command' >&2; exit 2 ;;
esac
`)
	d := &Docker{runtime: DockerRuntime}
	vm := testVM()
	ctx := context.Background()
	vm.ID = "missing"
	for i := 0; i < 2; i++ {
		if err := d.Stop(ctx, vm); err != nil {
			t.Fatal(err)
		}
		if err := d.Destroy(ctx, vm); err != nil {
			t.Fatal(err)
		}
	}
	vm.ID = "unavailable"
	if d.Stop(ctx, vm) == nil || d.Destroy(ctx, vm) == nil {
		t.Fatal("daemon failure hidden")
	}
	vm.ID = "running"
	if d.Stop(ctx, vm) == nil {
		t.Fatal("stop failure hidden")
	}
}
func TestContainerStartUnpausesAndChecksReadiness(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("TT_ENGINE_TEST_DIR", dir)
	fakeTool(t, dir, "podman", `case "$1" in
 inspect) /bin/cat "$TT_ENGINE_TEST_DIR/state" ;;
 start|unpause) echo "$1" > "$TT_ENGINE_TEST_DIR/action"; echo running > "$TT_ENGINE_TEST_DIR/state" ;;
 *) exit 2 ;;
esac
`)
	d := &Docker{runtime: PodmanRuntime}
	vm := testVM()
	for _, tc := range []struct{ state, action string }{{"paused", "unpause"}, {"exited", "start"}} {
		put(t, filepath.Join(dir, "state"), tc.state)
		if err := d.Start(context.Background(), vm); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "action"))
		if string(got) != tc.action+"\n" {
			t.Fatal(string(got))
		}
	}
	vm.Engine = model.Docker
	want := []string{"podman", "run", "-d", "--name", "tt-guest", "--cpus", "2", "--memory", "256m", "--memory-swap", "256m", "-p", "20022:22", "-p", "20080:80", "alpine"}
	if got := d.command(vm).Args; !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

func TestCreateAdoptsContainerCreatedBeforeALostResponse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TT_ENGINE_TEST_DIR", dir)
	t.Setenv("PATH", dir)
	fakeTool(t, dir, "docker", `for arg; do name="$arg"; done
case "$1:$2" in
 run:*) echo 'response lost after creation' >&2; : > "$TT_ENGINE_TEST_DIR/created"; exit 1 ;;
 inspect:-f)
  if [ -e "$TT_ENGINE_TEST_DIR/created" ]; then
   case "$3" in *Config.Image*) echo nginx ;; *) echo running ;; esac
   exit 0
  fi
  echo 'No such container' >&2; exit 1 ;;
 *) echo 'unexpected command' >&2; exit 2 ;;
esac
`)
	d := &Docker{runtime: DockerRuntime}
	vm := testVM()
	vm.Image = "nginx"
	if err := d.Create(context.Background(), vm, "nginx", "", nil); err != nil {
		t.Fatal(err)
	}
}
func TestCreateRejectsLeftoverContainerFromAnotherImage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TT_ENGINE_TEST_DIR", dir)
	t.Setenv("PATH", dir)
	fakeTool(t, dir, "docker", `for arg; do name="$arg"; done
case "$1:$2" in
 run:*) echo 'name conflict' >&2; exit 1 ;;
 inspect:-f)
  case "$3" in *Config.Image*) echo redis; exit 0 ;; *) echo running; exit 0 ;; esac ;;
 *) echo 'unexpected command' >&2; exit 2 ;;
esac
`)
	d := &Docker{runtime: DockerRuntime}
	vm := testVM()
	vm.Image = "nginx"
	err := d.Create(context.Background(), vm, "nginx", "", nil)
	if err == nil || !strings.Contains(err.Error(), "run failed") {
		t.Fatal(err)
	}
}
func TestStopUnpausesBeforeStoppingPausedContainers(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TT_ENGINE_TEST_DIR", dir)
	t.Setenv("PATH", dir)
	fakeTool(t, dir, "docker", `printf '%s\n' "$1" >> "$TT_ENGINE_TEST_DIR/calls"
case "$1" in
 inspect) echo paused ;;
 unpause) exit 0 ;;
 stop) exit 0 ;;
 *) exit 2 ;;
esac
`)
	d := &Docker{runtime: DockerRuntime}
	if err := d.Stop(context.Background(), testVM()); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if string(calls) != "inspect\nunpause\nstop\n" {
		t.Fatalf("%s", calls)
	}
}
