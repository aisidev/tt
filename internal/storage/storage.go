// Package storage manages image clones and verified backup artifacts.
package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"

	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
)

type ImageStore interface {
	CloneImage(context.Context, string, string) error
	RemoveImage(context.Context, string) error
	ListImages(context.Context, string) ([]string, error)
	ImageExists(context.Context, string) (bool, error)
	ResolveDisk(context.Context, string) (string, error)
	DiskFormat() string
	BhyveSize(context.Context, string) (uint64, error)
	QemuSize(context.Context, string) (uint64, error)
	ResizeDisk(context.Context, string, uint32) error
	FirecrackerDir(context.Context, string) (string, error)
	FirecrackerSize(context.Context, string) (uint64, error)
	ResizeFirecracker(context.Context, string, uint32) error
	Name() string
}

func New(kind model.Storage) (ImageStore, error) {
	switch kind {
	case model.File:
		return FileStore{}, nil
	case model.Zvol:
		return ZvolStore{}, nil
	default:
		return nil, fmt.Errorf("unknown storage backend: %s", kind)
	}
}
func ImageSizes(ctx context.Context, store ImageStore, root string, images []string) map[string]uint32 {
	result := map[string]uint32{}
	for _, name := range images {
		path := root + "/" + name
		size, err := store.FirecrackerSize(ctx, path)
		if err != nil {
			size, err = store.QemuSize(ctx, path)
		}
		if err != nil && runtime.GOOS == "freebsd" {
			size, err = store.BhyveSize(ctx, path)
		}
		mib := ceilMiB(size)
		if err == nil && mib > 0 && mib <= math.MaxUint32 {
			result[name] = uint32(mib)
		}
	}
	return result
}
func ceilMiB(bytes uint64) uint64 { return bytes/(1024*1024) + boolUint(bytes%(1024*1024) != 0) }
func boolUint(v bool) uint64 {
	if v {
		return 1
	}
	return 0
}
func qemuSize(ctx context.Context, store ImageStore, path string) (uint64, error) {
	disk, err := store.ResolveDisk(ctx, path)
	if err != nil {
		return 0, err
	}
	out, err := runcmd.Run(ctx, "qemu-img", "info", "--output=json", disk)
	if err != nil {
		return 0, fmt.Errorf("inspect QEMU disk: %w", err)
	}
	if !out.Success() {
		return 0, fmt.Errorf("cannot inspect QEMU disk")
	}
	var info map[string]json.RawMessage
	if err := json.Unmarshal(out.Stdout, &info); err != nil {
		return 0, fmt.Errorf("QEMU disk metadata: %w", err)
	}
	var format string
	_ = json.Unmarshal(info["format"], &format)
	if format != store.DiskFormat() {
		return 0, fmt.Errorf("unexpected QEMU disk format")
	}
	var size uint64
	if json.Unmarshal(info["virtual-size"], &size) != nil || size == 0 {
		return 0, fmt.Errorf("invalid QEMU disk size")
	}
	return size, nil
}
func fileBhyveSize(ctx context.Context, store ImageStore, path string) (uint64, error) {
	disk, err := store.ResolveDisk(ctx, path)
	if err != nil {
		return 0, err
	}
	info, err := os.Lstat(disk)
	if err != nil {
		return 0, fmt.Errorf("inspect bhyve disk: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return 0, fmt.Errorf("bhyve file storage requires a nonempty regular disk")
	}
	return uint64(info.Size()), nil
}
func fileFirecrackerSize(path string) (uint64, error) {
	info, err := os.Lstat(filepath.Join(path, "rootfs.ext4"))
	if err != nil {
		return 0, fmt.Errorf("inspect Firecracker rootfs: %w", err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("file storage requires a regular rootfs.ext4")
	}
	return uint64(info.Size()), nil
}
