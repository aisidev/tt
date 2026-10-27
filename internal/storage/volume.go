package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
)

// CreateRawVolume creates exclusively. Its caller must persist ownership and
// reserve capacity before invoking it; an existing path is never adopted.
func CreateRawVolume(ctx context.Context, store ImageStore, path string, sizeMiB uint32) error {
	if sizeMiB == 0 {
		return fmt.Errorf("volume size must be positive")
	}
	if store.Name() == "zvol" {
		if _, err := zfsCommand(ctx, "create", "-s", "-V", strconv.FormatUint(uint64(sizeMiB)*1024*1024, 10), "-o", "volmode=dev", path); err != nil {
			return err
		}
		zvol, ok := store.(ZvolStore)
		if !ok {
			return fmt.Errorf("unsupported ZFS volume store")
		}
		_, err := zvol.device(ctx, path)
		return err
	}
	if store.Name() != "file" {
		return fmt.Errorf("volume backend unsupported")
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err = file.Truncate(int64(sizeMiB) * 1024 * 1024); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func GrowRawVolume(ctx context.Context, store ImageStore, path string, sizeMiB uint32) error {
	if store.Name() == "zvol" {
		return store.ResizeDisk(ctx, path, sizeMiB)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("raw volume is not a regular file")
	}
	bytes := int64(sizeMiB) * 1024 * 1024
	if bytes < info.Size() {
		return fmt.Errorf("volume shrinking is forbidden")
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if err = file.Truncate(bytes); err != nil {
		return err
	}
	return file.Sync()
}

// SyncVolume makes a completed private file clone durable before its metadata
// can publish availability. Symlinks and special files are refused.
func SyncVolume(ctx context.Context, store ImageStore, path string, kind model.Engine) error {
	if store.Name() == "zvol" {
		return syncPool(ctx, strings.Split(path, "/")[0])
	}
	err := filepath.WalkDir(path, func(member string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.IsDir() {
			return syncDirectory(member)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("volume contains a non-regular member")
		}
		file, err := os.OpenFile(member, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer file.Close()
		return file.Sync()
	})
	if err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

// SnapshotVolume requires the caller to have fenced all writers and to hold a
// durable checkpoint identity. File checkpoints are independent immutable copies;
// ZFS checkpoints retain their source dataset until all dependents are removed.
func SnapshotVolume(ctx context.Context, store ImageStore, source, target, id string, kind model.Engine) error {
	if store.Name() == "file" {
		if err := store.CloneImage(ctx, source, target); err != nil {
			return err
		}
		return SyncVolume(ctx, store, target, kind)
	}
	args := []string{"snapshot"}
	if kind == model.Firecracker {
		args = append(args, "-r")
	}
	args = append(args, source+"@"+id)
	if _, err := zfsCommand(ctx, args...); err != nil {
		return err
	}
	return syncPool(ctx, strings.Split(source, "/")[0])
}
func RemoveVolumeSnapshot(ctx context.Context, store ImageStore, source, target, id string, kind model.Engine) error {
	if store.Name() == "file" {
		return store.RemoveImage(ctx, target)
	}
	snapshots, err := zfsCommand(ctx, "list", "-H", "-t", "snapshot", "-o", "name", "-r", source)
	if err != nil {
		return err
	}
	found := false
	for _, name := range strings.Split(snapshots, "\n") {
		if name == source+"@"+id {
			found = true
		}
	}
	if !found {
		return nil
	}
	args := []string{"destroy"}
	if kind == model.Firecracker {
		args = append(args, "-r")
	}
	args = append(args, source+"@"+id)
	_, err = zfsCommand(ctx, args...)
	return err
}
func CloneVolumeSnapshot(ctx context.Context, store ImageStore, source, fileSnapshot, target, id string, kind model.Engine) error {
	if store.Name() == "file" {
		return store.CloneImage(ctx, fileSnapshot, target)
	}
	if kind != model.Firecracker {
		if _, err := zfsCommand(ctx, "clone", "-o", "readonly=off", "-o", "volmode=dev", source+"@"+id, target); err != nil {
			return err
		}
		_, err := ResolveVolumeDevice(ctx, store, target)
		return err
	}
	index := strings.LastIndexByte(target, '/')
	if index < 0 {
		return fmt.Errorf("invalid target dataset")
	}
	mount, err := zfsMountpoint(ctx, target[:index])
	if err != nil {
		return err
	}
	if _, err = zfsCommand(ctx, "clone", "-o", "readonly=off", "-o", "mountpoint="+filepath.Join(mount, target[index+1:]), source+"@"+id, target); err != nil {
		return err
	}
	if _, err = zfsCommand(ctx, "clone", "-o", "readonly=off", "-o", "volmode=dev", source+"/rootfs@"+id, target+"/rootfs"); err != nil {
		return err
	}
	zvol, ok := store.(ZvolStore)
	if !ok {
		return fmt.Errorf("unsupported ZFS volume store")
	}
	return rewireZvolRoot(ctx, zvol, target, filepath.Join(mount, target[index+1:]))
}

func syncPool(ctx context.Context, pool string) error {
	result, err := runcmd.Run(ctx, "zpool", "sync", pool)
	if err != nil {
		return err
	}
	if !result.Success() {
		return fmt.Errorf("pool synchronization failed")
	}
	return nil
}

// A filesystem checkpoint contains the source's device symlink. Every clone
// must replace it with its own child device before the disk can become ready.
func rewireZvolRoot(ctx context.Context, store ZvolStore, dataset, dir string) error {
	device, err := store.device(ctx, dataset+"/rootfs")
	if err != nil {
		return err
	}
	link := filepath.Join(dir, "rootfs.ext4")
	if err = os.Remove(link); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(device, link)
}

// VolumeSource narrows file-backed QEMU imports to their selected disk and
// bounds copied Firecracker members by the reservation's metadata allowance.
func VolumeSource(ctx context.Context, store ImageStore, path string, kind model.Engine) (string, error) {
	if kind == model.Qemu && store.Name() == "file" {
		return store.ResolveDisk(ctx, path)
	}
	if kind != model.Firecracker {
		return path, nil
	}
	dir, err := store.FirecrackerDir(ctx, path)
	if err != nil {
		return "", err
	}
	members, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, member := range members {
		switch member.Name() {
		case "vmlinux":
			info, err := member.Info()
			if err != nil {
				return "", err
			}
			if !info.Mode().IsRegular() || info.Size() > 64<<20 {
				return "", fmt.Errorf("kernel exceeds the declared image envelope")
			}
		case "rootfs.ext4":
		case "guest-config.ext4":
			info, err := member.Info()
			if err != nil {
				return "", err
			}
			if !info.Mode().IsRegular() || info.Size() > int64(model.ConfigDiskMiB)<<20 {
				return "", fmt.Errorf("configuration disk exceeds its envelope")
			}
		default:
			return "", fmt.Errorf("root image contains unsupported members")
		}
	}
	return path, nil
}

func ResolveVolumeDevice(ctx context.Context, store ImageStore, path string) (string, error) {
	if zvol, ok := store.(ZvolStore); ok {
		return zvol.device(ctx, path)
	}
	return store.ResolveDisk(ctx, path)
}
