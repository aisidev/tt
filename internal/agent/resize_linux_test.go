package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aisidev/tt/internal/storage"
	"github.com/aisidev/tt/model"
)

func TestFirecrackerResizeRetryCompletesExt4Growth(t *testing.T) {
	if os.Getenv("TT_TEST_INTEGRATION") != "1" {
		t.Skip("real filesystem/tool integration is opt-in on an authorized host")
	}
	for _, tool := range []string{"mkfs.ext4", "e2fsck", "resize2fs", "dumpe2fs", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatal(err)
		}
	}
	r, _, _ := runtimeFixture(t)
	r.store = storage.FileStore{}
	ctx := context.Background()
	vm := &model.Vm{Backup: model.NewBackupState(), ID: "partial-ext4", EnvID: "env", HostID: "host", Image: "alpine", Engine: model.Firecracker, CPU: 1, Mem: 128, Disk: 16, State: model.VmStopped, Options: model.VmOptions{RequestedDisk: 16}}
	clone := r.clonePath(vm)
	if err := os.MkdirAll(clone, 0700); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(clone, "rootfs.ext4")
	f, err := os.Create(disk)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(16 * 1024 * 1024); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	contents := t.TempDir()
	if err := os.WriteFile(filepath.Join(contents, "marker"), []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mkfs.ext4", "-q", "-F", "-d", contents, disk).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v: %s", err, out)
	}
	if err := saveVM(ctx, r.db.conn, vm); err != nil {
		t.Fatal(err)
	}
	filesystemBytes := func() uint64 {
		t.Helper()
		out, err := exec.Command("dumpe2fs", "-h", disk).Output()
		if err != nil {
			t.Fatal(err)
		}
		fields := map[string]uint64{}
		for _, line := range strings.Split(string(out), "\n") {
			name, value, ok := strings.Cut(line, ":")
			if ok && (name == "Block count" || name == "Block size") {
				n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				fields[name] = n
			}
		}
		if len(fields) != 2 {
			t.Fatal("missing ext4 geometry", string(out))
		}
		return fields["Block count"] * fields["Block size"]
	}
	resize2fs, err := exec.LookPath("resize2fs")
	if err != nil {
		t.Fatal(err)
	}
	tools := t.TempDir()
	failure := filepath.Join(tools, "fail")
	if err := os.WriteFile(failure, nil, 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif [ -e \"$TT_RESIZE_FAIL\" ]; then echo 'injected resize2fs failure' >&2; exit 1; fi\nexec \"$TT_RESIZE_REAL\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(tools, "resize2fs"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TT_RESIZE_FAIL", failure)
	t.Setenv("TT_RESIZE_REAL", resize2fs)
	t.Setenv("PATH", tools+":"+os.Getenv("PATH"))
	target := model.VmResources{CPU: 1, Mem: 128, Disk: 32}
	for range 2 {
		if _, err := r.ResizeVM(ctx, vm.ID, target); err == nil {
			t.Fatal("incomplete ext4 growth reported success")
		}
		info, err := os.Stat(disk)
		if err != nil || info.Size() != 32*1024*1024 || filesystemBytes() != 16*1024*1024 {
			t.Fatal("fault did not leave the expected partial resize", info, err)
		}
		stored, err := loadVM(ctx, r.db.conn, vm.ID)
		if err != nil || stored.PendingResources == nil || stored.Disk != 16 {
			t.Fatal("partial resize lost its intent", stored, err)
		}
	}
	if err := os.Remove(failure); err != nil {
		t.Fatal(err)
	}
	updated, err := r.ResizeVM(ctx, vm.ID, target)
	if err != nil || updated.PendingResources != nil || updated.Error != nil || updated.Disk != 32 || filesystemBytes() != 32*1024*1024 {
		t.Fatal("retry did not complete the ext4 filesystem", updated, err)
	}
	data, err := exec.Command("debugfs", "-R", "cat /marker", disk).Output()
	if err != nil || string(data) != "retained" {
		t.Fatal("resize lost guest contents", string(data), err)
	}
}
