package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/aisidev/tt/internal/runcmd"
)

type FileStore struct{}

func (FileStore) Name() string       { return "file" }
func (FileStore) DiskFormat() string { return "qcow2" }
func (s FileStore) BhyveSize(ctx context.Context, path string) (uint64, error) {
	return fileBhyveSize(ctx, s, path)
}
func (s FileStore) QemuSize(ctx context.Context, path string) (uint64, error) {
	return qemuSize(ctx, s, path)
}
func (FileStore) FirecrackerDir(ctx context.Context, path string) (string, error) { return path, nil }
func (FileStore) FirecrackerSize(ctx context.Context, path string) (uint64, error) {
	return fileFirecrackerSize(path)
}
func (FileStore) ResizeFirecracker(ctx context.Context, path string, mib uint32) error {
	return ResizeExt4(ctx, filepath.Join(path, "rootfs.ext4"), mib)
}
func (FileStore) CloneImage(ctx context.Context, base, target string) error {
	meta, err := os.Lstat(base)
	if err != nil {
		return fmt.Errorf("inspect base image: %w", err)
	}
	source := base
	if meta.IsDir() {
		if err := os.Mkdir(target, 0700); err != nil {
			return fmt.Errorf("private clone directory: %w", err)
		}
		source = base + "/."
	} else if meta.Mode().IsRegular() {
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fmt.Errorf("private clone disk: %w", err)
		}
		f.Close()
	} else {
		return fmt.Errorf("base image must be a regular file or directory")
	}
	args := []string{"-R", source, target}
	if runtime.GOOS == "linux" {
		args = []string{"--reflink=auto", "-R", "--no-preserve=mode,ownership", source, target}
	}
	out, err := runcmd.Run(ctx, "cp", args...)
	if err != nil {
		// The placeholder above is known to be ours; a partial copy must not
		// wedge every retry behind EEXIST.
		if removeErr := os.RemoveAll(target); removeErr != nil {
			return fmt.Errorf("cp image: %w (cleanup of %s failed: %v)", err, target, removeErr)
		}
		return fmt.Errorf("cp image: %w", err)
	}
	if !out.Success() {
		if removeErr := os.RemoveAll(target); removeErr != nil {
			return fmt.Errorf("image copy failed: %s (cleanup of %s failed: %v)", out.Stderr, target, removeErr)
		}
		return fmt.Errorf("image copy failed: %s", out.Stderr)
	}
	return nil
}
func (FileStore) RemoveImage(ctx context.Context, path string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect image before removal: %w", err)
	}
	if info.IsDir() {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove dir: %w", err)
		}
	} else {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove file: %w", err)
		}
	}
	return nil
}
func (FileStore) ListImages(ctx context.Context, base string) ([]string, error) {
	info, err := os.Stat(base)
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect image catalog: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("image catalog is not a directory")
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, fmt.Errorf("read image dir: %w", err)
	}
	images := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "clone-") {
			images = append(images, name)
		}
	}
	slices.Sort(images)
	return images, nil
}
func (FileStore) ImageExists(ctx context.Context, path string) (bool, error) {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}
func (FileStore) ResolveDisk(ctx context.Context, path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect QEMU disk path: %w", err)
	}
	disk := path
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return "", fmt.Errorf("read QEMU image directory: %w", err)
		}
		var files, qcow []string
		for _, entry := range entries {
			meta, err := entry.Info()
			if err != nil {
				return "", fmt.Errorf("inspect QEMU image entry: %w", err)
			}
			if meta.Mode().IsRegular() || meta.Mode()&os.ModeSymlink != 0 {
				p := filepath.Join(path, entry.Name())
				files = append(files, p)
				if filepath.Ext(entry.Name()) == ".qcow2" && entry.Name() != ".qcow2" {
					qcow = append(qcow, p)
				}
			}
		}
		switch {
		case len(qcow) == 1:
			disk = qcow[0]
		case len(qcow) == 0 && len(files) == 1:
			disk = files[0]
		default:
			return "", fmt.Errorf("QEMU image directory must contain one unambiguous disk")
		}
	} else if !info.Mode().IsRegular() {
		return "", fmt.Errorf("QEMU disk must be a regular file, not a symbolic link")
	}
	info, err = os.Lstat(disk)
	if err != nil {
		return "", fmt.Errorf("inspect QEMU disk: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("QEMU disk must be a regular file, not a symbolic link")
	}
	return disk, nil
}
func (s FileStore) ResizeDisk(ctx context.Context, path string, mib uint32) error {
	disk, err := s.ResolveDisk(ctx, path)
	if err != nil {
		return err
	}
	out, err := runcmd.Run(ctx, "qemu-img", "info", "--output=json", disk)
	if err != nil {
		return fmt.Errorf("inspect disk: %w", err)
	}
	if !out.Success() {
		return fmt.Errorf("qemu-img info: %s", out.Stderr)
	}
	var info map[string]json.RawMessage
	if err := json.Unmarshal(out.Stdout, &info); err != nil {
		return fmt.Errorf("disk info: %w", err)
	}
	var size uint64
	if v, ok := info["virtual-size"]; !ok || string(v) == "null" || json.Unmarshal(v, &size) != nil {
		return fmt.Errorf("missing virtual disk size")
	}
	var format string
	if v, ok := info["format"]; !ok || string(v) == "null" || json.Unmarshal(v, &format) != nil {
		return fmt.Errorf("missing disk format")
	}
	if format != "qcow2" {
		return fmt.Errorf("file storage currently requires a qcow2 QEMU image")
	}
	requested := uint64(mib) * 1024 * 1024
	if requested < size {
		return fmt.Errorf("requested disk is smaller than the base image; choose at least %d MiB", ceilMiB(size))
	}
	if requested > size {
		out, err := runcmd.Run(ctx, "qemu-img", "resize", "-f", format, disk, strconv.FormatUint(requested, 10))
		if err != nil {
			return fmt.Errorf("resize disk: %w", err)
		}
		if !out.Success() {
			return fmt.Errorf("resize disk: %s", out.Stderr)
		}
	}
	return nil
}
func ResizeExt4(ctx context.Context, path string, mib uint32) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect ext4 clone: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("rootfs.ext4 must be a regular file")
	}
	requested := uint64(mib) * 1024 * 1024
	if requested < uint64(info.Size()) {
		return fmt.Errorf("requested disk is smaller than the current disk")
	}
	if err := checkExt4(ctx, path); err != nil {
		return err
	}
	disk, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open ext4 clone: %w", err)
	}
	defer disk.Close()
	if requested > uint64(info.Size()) {
		if err := disk.Truncate(int64(requested)); err != nil {
			return fmt.Errorf("grow ext4 clone: %w", err)
		}
	}
	if err := growExt4(ctx, path); err != nil {
		return err
	}
	if err := disk.Sync(); err != nil {
		return fmt.Errorf("sync ext4 clone: %w", err)
	}
	return nil
}
func checkExt4(ctx context.Context, path string) error {
	out, err := runcmd.Run(ctx, "e2fsck", "-f", "-p", path)
	if err != nil {
		return fmt.Errorf("check ext4 clone: %w", err)
	}
	if out.ExitCode != 0 && out.ExitCode != 1 {
		return fmt.Errorf("e2fsck failed: %s %s", out.Stdout, out.Stderr)
	}
	return nil
}
func growExt4(ctx context.Context, path string) error {
	out, err := runcmd.Run(ctx, "resize2fs", path)
	if err != nil {
		return fmt.Errorf("grow ext4 filesystem: %w", err)
	}
	if !out.Success() {
		return fmt.Errorf("resize2fs failed: %s", out.Stderr)
	}
	return nil
}
