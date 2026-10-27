package engine

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aisidev/tt/model"
)

func TestQemuSeedAndLaunchUseFakeTools(t *testing.T) {
	dir := t.TempDir()
	q := newQemu()
	q.runDir = dir
	q.processes = processes{root: t.TempDir()}
	vm := testVM()
	t.Setenv("PATH", dir)
	t.Setenv("TT_ENGINE_TEST_DIR", dir)
	isoScript := `printf '%s\n' "$@" > "$TT_ENGINE_TEST_DIR/iso-args"
output="$2"
for arg; do
 case "$arg" in */user-data) /bin/cp "$arg" "$TT_ENGINE_TEST_DIR/user-data" ;; */network-config) /bin/cp "$arg" "$TT_ENGINE_TEST_DIR/network-config" ;; esac
done
printf fake-iso > "$output"
`
	fakeTool(t, dir, "mkisofs", isoScript)
	fakeTool(t, dir, "genisoimage", isoScript)
	fakeTool(t, dir, "qemu-system-x86_64", `printf '%s\n' "$@" > "$TT_ENGINE_TEST_DIR/qemu-args"`)
	if err := q.Create(context.Background(), vm, "/retained/disk", "raw", []string{"ssh-ed25519 ABC hostile:\n<&>"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(q.seedPath(vm))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	if exists(filepath.Join(dir, "seed-guest")) {
		t.Fatal("staging directory retained on success")
	}
	args, _ := os.ReadFile(filepath.Join(dir, "qemu-args"))
	if !strings.Contains(string(args), "file=/retained/disk,format=raw,if=virtio\n") || !strings.Contains(string(args), "mac=02:54:0a:0a:01:02\n") || !strings.Contains(string(args), "readonly=on") {
		t.Fatal(string(args))
	}
	user, _ := os.ReadFile(filepath.Join(dir, "user-data"))
	if !strings.Contains(string(user), `"ssh-ed25519 ABC hostile:\n<&>"`) {
		t.Fatal(string(user))
	}
	fakeTool(t, dir, "mkisofs", "echo failure >&2; exit 1\n")
	fakeTool(t, dir, "genisoimage", "echo failure >&2; exit 1\n")
	if err := q.generateSeedISO(context.Background(), vm, nil); err == nil {
		t.Fatal("failed seed accepted")
	}
	content, _ := os.ReadFile(q.seedPath(vm))
	if string(content) != "fake-iso" {
		t.Fatal("previous seed lost")
	}
}
func TestQemuManagedSSHSeedUsesSerdeQuoting(t *testing.T) {
	q := newQemu()
	q.runDir = t.TempDir()
	vm := testVM()
	vm.Options.SSH = &model.SshOptions{}
	put(t, filepath.Join(q.runDir, "ssh-guest.sh"), "#!/bin/sh\necho '<&>\u2028'\n")
	got, err := q.userData(vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "    content: \"#!/bin/sh\\necho '<&>\u2028'\\n\"\n") {
		t.Fatal(got)
	}
}
func TestQemuHMPMonitorHandlesSplitPromptsAndPause(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "tthmp-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	q := newQemu()
	q.runDir = dir
	q.processes = processes{root: t.TempDir()}
	vm := testVM()
	fakeProcess(t, q.processes, 24, "qemu\x00"+q.pidPath(vm)+"\x00")
	listener, err := net.Listen("unix", q.monitorPath(vm))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	result := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			result <- err
			return
		}
		defer conn.Close()
		conn.Write([]byte("QEMU monitor\r\n(qe"))
		conn.Write([]byte("mu) "))
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err == nil && line != "info status\n" {
			result <- &unexpectedCommand{line}
			return
		}
		if err == nil {
			_, err = conn.Write([]byte("VM status: paused\r\n(qemu) "))
		}
		result <- err
	}()
	state, err := q.State(context.Background(), vm)
	if err != nil || state != model.VmPaused {
		t.Fatal(state, err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

type unexpectedCommand struct{ command string }

func (e *unexpectedCommand) Error() string { return "unexpected HMP command: " + e.command }
