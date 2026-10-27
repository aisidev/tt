package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aisidev/tt/internal/runcmd"
)

const CloneSnapshot = "ttsnap"

type ZvolStore struct {
	waitDevice func(context.Context, string) (string, error)
}

func (ZvolStore) Name() string       { return "zvol" }
func (ZvolStore) DiskFormat() string { return "raw" }
func (s ZvolStore) QemuSize(ctx context.Context, path string) (uint64, error) {
	return qemuSize(ctx, s, path)
}
func zfsCommand(ctx context.Context, args ...string) (string, error) {
	out, err := runcmd.Run(ctx, "zfs", args...)
	if err != nil {
		return "", fmt.Errorf("failed to execute zfs: %w", err)
	}
	if !out.Success() {
		return "", fmt.Errorf("zfs %s failed: %s", args[0], strings.TrimSpace(string(out.Stderr)))
	}
	return strings.TrimSpace(string(out.Stdout)), nil
}
func zfsOK(ctx context.Context, args ...string) bool {
	out, err := runcmd.Run(ctx, "zfs", args...)
	return err == nil && out.Success()
}
func zfsProperty(ctx context.Context, dataset, name string) (string, error) {
	return zfsCommand(ctx, "get", "-Hp", "-o", "value", name, dataset)
}
func zfsMountpoint(ctx context.Context, dataset string) (string, error) {
	path, err := zfsProperty(ctx, dataset, "mountpoint")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("ZFS dataset %s must have an active absolute mountpoint", dataset)
	}
	mounted, err := zfsProperty(ctx, dataset, "mounted")
	if err != nil {
		return "", err
	}
	if mounted != "yes" {
		return "", fmt.Errorf("ZFS dataset %s must have an active absolute mountpoint", dataset)
	}
	return path, nil
}
func zvolSize(ctx context.Context, dataset string) (uint64, error) {
	text, err := zfsProperty(ctx, dataset, "volsize")
	if err != nil {
		return 0, err
	}
	size, err := strconv.ParseUint(strings.TrimPrefix(text, "+"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("zvol size: %w", err)
	}
	return size, nil
}
func (s ZvolStore) device(ctx context.Context, dataset string) (string, error) {
	if s.waitDevice != nil {
		return s.waitDevice(ctx, dataset)
	}
	path := "/dev/zvol/" + dataset
	started := time.Now()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := os.Stat(path)
		if err == nil && info.Mode()&os.ModeDevice != 0 {
			character := info.Mode()&os.ModeCharDevice != 0
			if character == (runtime.GOOS == "freebsd") {
				return path, nil
			}
		}
		if time.Since(started) >= 5*time.Second {
			return "", fmt.Errorf("ZFS volume device did not appear: %s", path)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}
func ensureCloneSnapshot(ctx context.Context, dataset string, recursive bool) error {
	snap := dataset + "@" + CloneSnapshot
	if !zfsOK(ctx, "list", "-t", "snapshot", snap) {
		args := []string{"snapshot"}
		if recursive {
			args = append(args, "-r")
		}
		args = append(args, snap)
		if _, err := zfsCommand(ctx, args...); err != nil {
			return err
		}
	}
	if recursive && !zfsOK(ctx, "list", "-t", "snapshot", dataset+"/rootfs@"+CloneSnapshot) {
		return fmt.Errorf("incomplete Firecracker base snapshot; import a new image revision")
	}
	return nil
}
func (s ZvolStore) CloneImage(ctx context.Context, base, target string) (err error) {
	// Datasets created by this invocation, newest first on rollback. A partial
	// clone must be retryable, so any failure destroys what we made; ownership
	// is certain because the clone commands create them exclusively.
	created := []string{}
	rollback := func() {
		for i := len(created) - 1; i >= 0; i-- {
			if _, destroyErr := zfsCommand(ctx, "destroy", "-R", created[i]); destroyErr != nil {
				err = fmt.Errorf("%w; failed to roll back %s: %v", err, created[i], destroyErr)
			}
		}
	}
	failed := true
	defer func() {
		if failed {
			rollback()
		}
	}()
	kind, err := zfsProperty(ctx, base, "type")
	if err != nil {
		return err
	}
	fc := kind == "filesystem"
	if fc {
		if _, err := s.FirecrackerSize(ctx, base); err != nil {
			return err
		}
	}
	if err := ensureCloneSnapshot(ctx, base, fc); err != nil {
		return err
	}
	snap := base + "@" + CloneSnapshot
	if fc {
		index := strings.LastIndexByte(target, '/')
		if index < 0 {
			return fmt.Errorf("invalid clone dataset")
		}
		mount, err := zfsMountpoint(ctx, target[:index])
		if err != nil {
			return err
		}
		dir := mount + "/" + target[index+1:]
		if _, err := zfsCommand(ctx, "clone", "-o", "mountpoint="+dir, "-o", "readonly=off", "-o", "canmount=on", snap, target); err != nil {
			return err
		}
		created = append(created, target)
		if _, err := zfsCommand(ctx, "clone", "-o", "readonly=off", "-o", "volmode=dev", base+"/rootfs@"+CloneSnapshot, target+"/rootfs"); err != nil {
			return err
		}
		created = append(created, target+"/rootfs")
		if err := rewireZvolRoot(ctx, s, target, dir); err != nil {
			return err
		}
	} else {
		if _, err := zfsCommand(ctx, "clone", "-o", "readonly=off", "-o", "volmode=dev", snap, target); err != nil {
			return err
		}
		created = append(created, target)
		if _, err := s.device(ctx, target); err != nil {
			return err
		}
	}
	failed = false
	return nil
}
func (ZvolStore) RemoveImage(ctx context.Context, path string) error {
	index := strings.LastIndexByte(path, '/')
	if index < 0 {
		return fmt.Errorf("expected dataset/clone name")
	}
	children, err := zfsCommand(ctx, "list", "-H", "-o", "name", "-r", path[:index])
	if err != nil {
		return err
	}
	for _, name := range strings.Split(children, "\n") {
		if name == path {
			_, err := zfsCommand(ctx, "destroy", "-r", path)
			return err
		}
	}
	return nil
}
func (s ZvolStore) ListImages(ctx context.Context, base string) ([]string, error) {
	out, err := zfsCommand(ctx, "list", "-H", "-o", "name,type", "-d", "1", base)
	if err != nil {
		return nil, err
	}
	images := []string{}
	for _, line := range strings.Split(out, "\n") {
		name, kind, ok := strings.Cut(line, "\t")
		if !ok || name == base {
			continue
		}
		include := kind == "volume"
		if kind == "filesystem" {
			_, err := s.FirecrackerSize(ctx, name)
			include = err == nil
		}
		if include {
			parts := strings.Split(name, "/")
			images = append(images, parts[len(parts)-1])
		}
	}
	slices.Sort(images)
	return images, nil
}
func (ZvolStore) ImageExists(ctx context.Context, path string) (bool, error) {
	index := strings.LastIndexByte(path, '/')
	if index < 0 {
		return false, fmt.Errorf("expected parent dataset and image name")
	}
	names, err := zfsCommand(ctx, "list", "-H", "-o", "name", "-r", "-d", "1", path[:index])
	if err != nil {
		return false, err
	}
	for _, name := range strings.Split(names, "\n") {
		if name == path {
			return true, nil
		}
	}
	return false, nil
}
func (ZvolStore) ResolveDisk(ctx context.Context, path string) (string, error) {
	return "/dev/zvol/" + path, nil
}
func (ZvolStore) ResizeDisk(ctx context.Context, path string, mib uint32) error {
	current, err := zvolSize(ctx, path)
	if err != nil {
		return err
	}
	requested := uint64(mib) * 1024 * 1024
	if requested < current {
		return fmt.Errorf("requested disk is smaller than base zvol")
	}
	if requested > current {
		_, err := zfsCommand(ctx, "set", fmt.Sprintf("volsize=%d", requested), path)
		return err
	}
	return nil
}
func (ZvolStore) BhyveSize(ctx context.Context, path string) (uint64, error) {
	kind, err := zfsProperty(ctx, path, "type")
	if err != nil {
		return 0, err
	}
	if kind != "volume" {
		return 0, fmt.Errorf("bhyve requires a root zvol")
	}
	bytes, err := zvolSize(ctx, path)
	if err != nil {
		return 0, err
	}
	if bytes == 0 {
		return 0, fmt.Errorf("bhyve requires a nonempty root zvol")
	}
	return bytes, nil
}
func (s ZvolStore) FirecrackerDir(ctx context.Context, path string) (string, error) {
	dir, err := zfsMountpoint(ctx, path)
	if err != nil {
		return "", err
	}
	if _, err := s.device(ctx, path+"/rootfs"); err != nil {
		return "", err
	}
	return dir, nil
}
func (ZvolStore) FirecrackerSize(ctx context.Context, path string) (uint64, error) {
	dir, err := zfsMountpoint(ctx, path)
	if err != nil {
		return 0, err
	}
	info, err := os.Lstat(filepath.Join(dir, "vmlinux"))
	if err != nil || !info.Mode().IsRegular() {
		//lint:ignore ST1005 Preserve the pinned compatibility diagnostic.
		return 0, fmt.Errorf("Firecracker ZFS image requires a regular vmlinux")
	}
	return zvolSize(ctx, path+"/rootfs")
}
func (s ZvolStore) ResizeFirecracker(ctx context.Context, path string, mib uint32) error {
	volume := path + "/rootfs"
	size, err := zvolSize(ctx, volume)
	if err != nil {
		return err
	}
	if uint64(mib)*1024*1024 < size {
		return fmt.Errorf("requested disk is smaller than the current zvol")
	}
	disk, err := s.device(ctx, volume)
	if err != nil {
		return err
	}
	if err := checkExt4(ctx, disk); err != nil {
		return err
	}
	if err := s.ResizeDisk(ctx, volume, mib); err != nil {
		return err
	}
	return growExt4(ctx, disk)
}
