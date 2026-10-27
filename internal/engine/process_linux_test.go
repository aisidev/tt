package engine

import (
	"context"
	"github.com/aisidev/tt/internal/netcfg"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fakeProcess(t *testing.T, p processes, pid uint32, args string) {
	t.Helper()
	dir := filepath.Join(p.root, strconv.FormatUint(uint64(pid), 10))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(dir, "cmdline"), args)
}
func TestPIDRecoveryRetainsUnknownOutcomesAndRejectsReuse(t *testing.T) {
	p := processes{root: t.TempDir()}
	path := filepath.Join(t.TempDir(), "vm.pid")
	marker := "/owned.sock"
	fakeProcess(t, p, 23, "unrelated\x00/owned.sock.extra\x00")
	put(t, path, "23")
	if pid, err := p.recover(path, []string{marker}); err != nil || pid != 0 {
		t.Fatal(pid, err)
	}
	fakeProcess(t, p, 24, "vmm\x00"+marker+"\x00")
	if pid, err := p.recover(path, []string{marker}); err != nil || pid != 24 {
		t.Fatal(pid, err)
	}
	fakeProcess(t, p, 25, "vmm\x00"+marker+"\x00")
	if _, err := p.recover(path, []string{marker}); err == nil {
		t.Fatal("duplicate identity accepted")
	}
	if err := os.Remove(filepath.Join(p.root, "25", "cmdline")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(p.root, "25", "cmdline"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := p.recover(path, []string{marker}); err == nil {
		t.Fatal("unreadable inventory hidden")
	}
	fakeProcess(t, p, 23, "")
	put(t, filepath.Join(p.root, "23", "stat"), "23 (vmm) R 1 2 3")
	if _, err := p.matches(23, marker); err == nil {
		t.Fatal("empty live identity treated as absence")
	}
	put(t, filepath.Join(p.root, "23", "stat"), "23 (name with ) parens) Z 1 2 3")
	if match, err := p.matches(23, marker); err != nil || match {
		t.Fatal(match, err)
	}
}
func TestCorruptAndMissingPIDFilesRecoverTheExactProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vm.pid")
	marker := filepath.Join(dir, "specific-vm.sock")
	cmd := exec.Command("sleep", "20")
	cmd.Args[0] = marker
	child, err := startChild(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer child.kill()
	p := hostProcesses()
	pid := uint32(cmd.Process.Pid)
	deadline := time.Now().Add(5 * time.Second)
	for {
		match, err := p.matches(pid, marker)
		if err == nil && match {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("identity not established", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A stale PID pointing at our own live helper with a different marker must not
	// send a signal. This test never addresses any unrelated host process.
	if err := p.terminate(context.Background(), pid, "another-vm"); err != nil {
		t.Fatal(err)
	}
	if exited, _ := child.exited(); exited {
		t.Fatal("PID reuse killed helper")
	}
	put(t, path, "truncated")
	for i := 0; i < 2; i++ {
		got, err := p.recover(path, []string{marker})
		if err != nil || got != pid {
			t.Fatal(got, err)
		}
		_ = os.Remove(path)
	}
	// Linux can clear cmdline before publishing the zombie state. That is an
	// unknown observation, so production correctly retains resources and asks
	// for a retry. Our owned child completion confirms when that retry is safe.
	if err := p.terminate(context.Background(), pid, marker); err != nil && !strings.Contains(err.Error(), "has no readable identity; resources retained") {
		t.Fatal(err)
	}
	select {
	case <-child.done:
	case <-time.After(5 * time.Second):
		t.Fatal("owned child did not exit after termination")
	}
	if err := p.terminate(context.Background(), pid, marker); err != nil {
		t.Fatal(err)
	}
	if pid, err := p.recover(path, []string{marker}); err != nil || pid != 0 {
		t.Fatal(pid, err)
	}
}
func TestMissingFirecrackerMetadataPreventsLifecycleAndRestore(t *testing.T) {
	f := newFirecracker()
	f.runDir = t.TempDir()
	f.processes = processes{root: t.TempDir()}
	vm := testVM()
	fakeProcess(t, f.processes, 24, "firecracker\x00/fc-"+netcfg.TapName(vm.ID)+".sock\x00")
	_, _, err := f.identity(vm)
	if err == nil || !strings.Contains(err.Error(), "sandbox metadata is missing") {
		t.Fatal(err)
	}
	if err := f.PrepareDiskRestore(context.Background(), vm); err == nil {
		t.Fatal("missing metadata allowed restore")
	}
}

func TestNonUTF8PIDMetadataRetainsResourcesInsteadOfScanning(t *testing.T) {
	p := processes{root: t.TempDir()}
	path := filepath.Join(t.TempDir(), "vm.pid")
	marker := "/owned.sock"
	for _, running := range []bool{false, true} {
		if running {
			fakeProcess(t, p, 23, "vmm\x00"+marker+"\x00")
		}
		put(t, path, "\xff23")
		if pid, err := p.recover(path, []string{marker}); err == nil || pid != 0 || !strings.Contains(err.Error(), "read VM PID") {
			t.Fatal(running, pid, err)
		}
	}
	fakeProcess(t, p, 23, "")
	put(t, filepath.Join(p.root, "23", "stat"), "23 (\xff) Z 1 2 3")
	if _, err := p.matches(23, marker); err == nil {
		t.Fatal("invalid process metadata treated as confirmed exit")
	}
}
