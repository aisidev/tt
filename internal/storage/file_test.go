package storage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/aisidev/tt/model"
)

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func fakeTool(t *testing.T, name, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}
func requireIntegration(t *testing.T, tools ...string) {
	t.Helper()
	if os.Getenv("TT_TEST_INTEGRATION") != "1" {
		t.Skip("opt-in real filesystem/tool integration on authorized host")
	}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatal(err)
		}
	}
}
func TestCreateStoreNames(t *testing.T) {
	for _, kind := range []model.Storage{model.File, model.Zvol} {
		store, err := New(kind)
		if err != nil || store.Name() != kind.String() {
			t.Fatal(store, err)
		}
	}
}
func TestBhyveCapacityRejectsEmptyAmbiguousAndLinkedDisks(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.raw")
	writeTestFile(t, disk, nil)
	store := FileStore{}
	if _, err := store.BhyveSize(ctx, dir); err == nil {
		t.Fatal("empty disk")
	}
	writeTestFile(t, disk, make([]byte, 1025))
	if size, err := store.BhyveSize(ctx, dir); err != nil || size != 1025 {
		t.Fatal(size, err)
	}
	other := filepath.Join(dir, "second.raw")
	writeTestFile(t, other, make([]byte, 2048))
	if _, err := store.BhyveSize(ctx, dir); err == nil {
		t.Fatal("ambiguous directory")
	}
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(disk, other); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BhyveSize(ctx, other); err == nil {
		t.Fatal("linked disk")
	}
}
func TestResolveDiskPrefersOneQCOW2AndRejectsLinks(t *testing.T) {
	dir := t.TempDir()
	store := FileStore{}
	ctx := context.Background()
	writeTestFile(t, filepath.Join(dir, "notes"), []byte("metadata"))
	disk := filepath.Join(dir, "disk.qcow2")
	writeTestFile(t, disk, []byte("disk"))
	if got, err := store.ResolveDisk(ctx, dir); err != nil || got != disk {
		t.Fatal(got, err)
	}
	writeTestFile(t, filepath.Join(dir, "other.qcow2"), nil)
	if _, err := store.ResolveDisk(ctx, dir); err == nil {
		t.Fatal("ambiguous qcow2")
	}
	if err := os.Remove(filepath.Join(dir, "other.qcow2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(disk); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "notes"), disk); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveDisk(ctx, dir); err == nil {
		t.Fatal("symlink silently skipped")
	}
}
func TestListImagesPreservesRepositoryContents(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"b", "a", "clone-owned", ".hidden"} {
		writeTestFile(t, filepath.Join(dir, name), nil)
	}
	got, err := (FileStore{}).ListImages(context.Background(), dir)
	if err != nil || !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatal(got, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 4 {
		t.Fatal("catalog changed files")
	}
}
func TestQEMUResizeRejectsShrinkAndUnknownFormats(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.qcow2")
	writeTestFile(t, disk, nil)
	log := filepath.Join(dir, "calls")
	t.Setenv("TT_STORAGE_LOG", log)
	fakeTool(t, "qemu-img", `printf '%s\n' "$*" >> "$TT_STORAGE_LOG"
if [ "$1" = info ]; then printf '%s\n' '{"format":"qcow2","virtual-size":1048576}'; fi
`)
	store := FileStore{}
	ctx := context.Background()
	if err := store.ResizeDisk(ctx, disk, 0); err == nil || !strings.Contains(err.Error(), "choose at least 1 MiB") {
		t.Fatal(err)
	}
	if err := store.ResizeDisk(ctx, disk, 2); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "resize -f qcow2 "+disk+" 2097152") {
		t.Fatal(string(calls))
	}
}
func TestPublicBaseImagesProducePrivateClones(t *testing.T) {
	requireIntegration(t, "cp")
	dir := t.TempDir()
	for _, directory := range []bool{false, true} {
		suffix := "file"
		if directory {
			suffix = "dir"
		}
		base := filepath.Join(dir, "base-"+suffix)
		target := filepath.Join(dir, "clone-"+suffix)
		mode := os.FileMode(0644)
		if directory {
			mode = 0755
			if err := os.Mkdir(base, 0755); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(base, "disk.qcow2"), []byte("guest"))
		} else {
			writeTestFile(t, base, []byte("guest"))
		}
		if err := os.Chmod(base, mode); err != nil {
			t.Fatal(err)
		}
		if err := (FileStore{}).CloneImage(context.Background(), base, target); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(target)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatal(info, err)
		}
		original, _ := os.Stat(base)
		if original.Mode().Perm()&0077 == 0 {
			t.Fatal("base permissions changed")
		}
	}
}
func TestExt4GrowthPreservesFilesAndRejectsShrinking(t *testing.T) {
	requireIntegration(t, "mkfs.ext4", "debugfs", "e2fsck", "resize2fs", "dumpe2fs", "cp")
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	clone := filepath.Join(dir, "clone.ext4")
	contents := filepath.Join(dir, "contents")
	if err := os.Mkdir(contents, 0700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(contents, "marker"), []byte("persistent-data"))
	f, err := os.Create(base)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(64 * 1024 * 1024)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if data, err := exec.Command("mkfs.ext4", "-q", "-F", "-d", contents, base).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	if err := (FileStore{}).CloneImage(context.Background(), base, clone); err != nil {
		t.Fatal(err)
	}
	if err := ResizeExt4(context.Background(), clone, 96); err != nil {
		t.Fatal(err)
	}
	data, err := exec.Command("debugfs", "-R", "cat /marker", clone).Output()
	if err != nil || string(data) != "persistent-data" {
		t.Fatalf("%q %v", data, err)
	}
	if err := ResizeExt4(context.Background(), clone, 64); err == nil {
		t.Fatal("shrink accepted")
	}
	// Resume an interrupted filesystem growth after the device has already grown.
	if err := os.Truncate(clone, 128*1024*1024); err != nil {
		t.Fatal(err)
	}
	if err := ResizeExt4(context.Background(), clone, 128); err != nil {
		t.Fatal(err)
	}
	data, err = exec.Command("dumpe2fs", "-h", clone).Output()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && (key == "Block count" || key == "Block size") {
			n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			values[key] = n
		}
	}
	if values["Block count"]*values["Block size"] != 128*1024*1024 {
		t.Fatal("filesystem growth incomplete", values)
	}
	baseInfo, _ := os.Stat(base)
	cloneInfo, _ := os.Stat(clone)
	if baseInfo.Size() != 64*1024*1024 || cloneInfo.Size() != 128*1024*1024 {
		t.Fatal("wrong disk sizes")
	}
}

func TestFailedCloneCloneLeavesNoPlaceholder(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "first-attempt")
	t.Setenv("TT_COPY_MARKER", marker)
	fakeTool(t, "cp", `if [ ! -e "$TT_COPY_MARKER" ]; then
 touch "$TT_COPY_MARKER"
 printf 'simulated partial copy failure\n' >&2
 exit 1
fi
exec /bin/cp "$@"
`)
	base := filepath.Join(dir, "base.qcow2")
	writeTestFile(t, base, []byte("image-bytes"))
	target := filepath.Join(dir, "clone-vm")
	if err := (FileStore{}).CloneImage(context.Background(), base, target); err == nil {
		t.Fatal("first attempt succeeded")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("partial clone left behind", err)
	}
	if err := (FileStore{}).CloneImage(context.Background(), base, target); err != nil {
		t.Fatal("retry did not recover", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "image-bytes" {
		t.Fatal(string(data), err)
	}
}
