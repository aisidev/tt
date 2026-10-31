package guestconfig

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
)

func writeDisk(ctx context.Context, directory string, files model.GuestConfig) error {
	if err := Validate(model.Firecracker, files, false); err != nil {
		return err
	}
	destination := filepath.Join(directory, ConfigDisk)
	if len(files) == 0 {
		err := os.Remove(destination)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	contents, err := os.MkdirTemp(directory, ".config-files-*")
	if err != nil {
		return fmt.Errorf("config staging: %w", err)
	}
	defer os.RemoveAll(contents)
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(contents, name), []byte(value), 0400); err != nil {
			return fmt.Errorf("write guest config: %w", err)
		}
	}
	disk, err := os.CreateTemp(directory, ".config-disk-*")
	if err != nil {
		return fmt.Errorf("config disk staging: %w", err)
	}
	defer disk.Close()
	defer os.Remove(disk.Name())
	if err := disk.Truncate(int64(model.ConfigDiskMiB) * 1024 * 1024); err != nil {
		return fmt.Errorf("config disk size: %w", err)
	}
	output, err := runcmd.Run(ctx, "mkfs.ext4", "-q", "-F", "-L", "TTCONFIG", "-d", contents, disk.Name())
	if err != nil {
		return fmt.Errorf("build config disk: %w", err)
	}
	if !output.Success() {
		return fmt.Errorf("mkfs.ext4 failed to build guest configuration disk")
	}
	if err := disk.Sync(); err != nil {
		return fmt.Errorf("sync config disk: %w", err)
	}
	if err := os.Rename(disk.Name(), destination); err != nil {
		return err
	}
	parent, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
