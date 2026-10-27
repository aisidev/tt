package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aisidev/tt/internal/netcfg"
	"github.com/aisidev/tt/model"
	"golang.org/x/sys/unix"
)

type sandbox struct {
	Root   string `json:"root"`
	Marker string `json:"marker"`
	Cgroup string `json:"cgroup"`
}

func (s *sandbox) UnmarshalJSON(data []byte) error {
	type plain sandbox
	return model.DecodeRecord(data, (*plain)(s), []string{"root", "marker", "cgroup"}, false)
}
func (s *sandbox) socket() string { return filepath.Join(s.Root, strings.TrimLeft(s.Marker, "/")) }
func (s *sandbox) cleanup() error {
	parent, ok := parentPath(s.Root)
	if !ok {
		return fmt.Errorf("invalid sandbox path")
	}
	if err := os.RemoveAll(parent); err != nil {
		return fmt.Errorf("remove Firecracker sandbox: %w", err)
	}
	// Remove only the empty cgroup directory, never its members recursively.
	if err := unix.Rmdir(s.Cgroup); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove Firecracker cgroup: %w", err)
	}
	return nil
}
func (f *Firecracker) sandboxPath(vm *model.Vm) string {
	return filepath.Join(f.runDir, "fc-"+vm.ID+".sandbox.json")
}
func (f *Firecracker) loadSandbox(vm *model.Vm) (*sandbox, error) {
	data, err := os.ReadFile(f.sandboxPath(vm))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Firecracker sandbox: %w", err)
	}
	var s sandbox
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("read Firecracker sandbox: %w", err)
	}
	return &s, nil
}
func stageMember(source, target string, writable bool) error {
	return stageMemberWithDevice(source, target, writable, unix.Stat, unix.Mknod)
}

// The syscall boundary is explicit so tests can verify device identity without
// creating a block device on the developer workstation.
func stageMemberWithDevice(source, target string, writable bool, statDevice func(string, *unix.Stat_t) error, makeDevice func(string, uint32, int) error) error {
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("guest image member: %w", err)
	}
	if info.Mode().IsRegular() {
		err := os.Link(source, target)
		if err == nil {
			return nil
		}
		if writable || !errors.Is(err, unix.EXDEV) {
			return fmt.Errorf("link persistent jail member: %w", err)
		}
		src, err := os.Open(source)
		if err != nil {
			return fmt.Errorf("copy read-only jail member: %w", err)
		}
		defer src.Close()
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return fmt.Errorf("copy read-only jail member: %w", err)
		}
		_, copyErr := io.Copy(dst, src)
		closeErr := dst.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return fmt.Errorf("copy read-only jail member: %w", err)
		}
		return nil
	}
	var stat unix.Stat_t
	if err := statDevice(source, &stat); err != nil {
		return fmt.Errorf("resolve root block device: %w", err)
	}
	if !writable || stat.Mode&unix.S_IFMT != unix.S_IFBLK {
		return fmt.Errorf("Firecracker requires regular kernel/config files and a regular or block root disk")
	}
	if err := makeDevice(target, unix.S_IFBLK|0600, int(stat.Rdev)); err != nil {
		return fmt.Errorf("create jailed root block device: %w", err)
	}
	return nil
}
func (f *Firecracker) prepareSandbox(ctx context.Context, vm *model.Vm, image string) (*sandbox, *exec.Cmd, error) {
	binary, err := executable("firecracker")
	if err != nil {
		return nil, nil, err
	}
	jailer, err := executable("jailer")
	if err != nil {
		return nil, nil, err
	}
	if !exists(filepath.Join(f.cgroupRoot, "cgroup.controllers")) {
		return nil, nil, fmt.Errorf("Firecracker sandbox requires cgroup v2")
	}
	image, err = canonicalPath(image)
	if err != nil {
		return nil, nil, fmt.Errorf("Firecracker image path: %w", err)
	}
	parent, ok := parentPath(image)
	if !ok {
		return nil, nil, fmt.Errorf("invalid image path")
	}
	id := netcfg.TapName(vm.ID)
	base := filepath.Join(parent, ".jailer")
	parentGroup := "ttstack"
	if f.controlParent != "" {
		parentGroup = f.controlParent
	}
	s := &sandbox{Root: filepath.Join(base, filepath.Base(binary), id, "root"), Marker: "/fc-" + id + ".sock", Cgroup: filepath.Join(f.cgroupRoot, parentGroup, id)}
	if len(s.socket()) >= 108 {
		return nil, nil, fmt.Errorf("runtime directory is too long for a Firecracker Unix socket; use a shorter --runtime-dir")
	}
	if err := os.MkdirAll(s.Root, 0777); err != nil {
		return nil, nil, fmt.Errorf("create Firecracker sandbox: %w", err)
	}
	// The jailer uid is derived from the guest address; only the managed
	// subnet maps into the reserved 100000+ block, so bound the input to it.
	ip, err := netcfg.FleetIPv4(vm.IP)
	if err != nil {
		return nil, nil, err
	}
	octets := ip.As4()
	uid := uint32(100000) + uint32(octets[2])*256 + uint32(octets[3])
	data, err := model.SerdeJSON(s)
	if err != nil {
		return nil, nil, err
	}
	if err := privateWrite(f.sandboxPath(vm), data); err != nil {
		return nil, nil, err
	}
	members := []string{"vmlinux", "rootfs.ext4"}
	if vm.Options.ConfigDiskMiB(vm.Engine) > 0 {
		members = append(members, "guest-config.ext4")
	}
	for _, name := range members {
		target := filepath.Join(s.Root, name)
		if err := removeFile(target); err != nil {
			return nil, nil, err
		}
		if err := stageMember(filepath.Join(image, name), target, name == "rootfs.ext4"); err != nil {
			return nil, nil, err
		}
		if err := f.chown(target, int(uid), int(uid)); err != nil {
			return nil, nil, fmt.Errorf("guest file ownership: %w", err)
		}
		mode := os.FileMode(0400)
		if name == "rootfs.ext4" {
			mode = 0600
		}
		if err := os.Chmod(target, mode); err != nil {
			return nil, nil, fmt.Errorf("guest file permissions: %w", err)
		}
	}
	for index, disk := range vm.Options.DataVolumes {
		if disk.Format != "raw" {
			return nil, nil, fmt.Errorf("data volume must use raw format")
		}
		target := filepath.Join(s.Root, fmt.Sprintf("data-%d.raw", index))
		if err := removeFile(target); err != nil {
			return nil, nil, err
		}
		if err := stageMember(disk.Path, target, true); err != nil {
			return nil, nil, err
		}
		if err := f.chown(target, int(uid), int(uid)); err != nil {
			return nil, nil, err
		}
		if err := os.Chmod(target, 0600); err != nil {
			return nil, nil, err
		}
	}
	if err := removeFile(s.socket()); err != nil {
		return nil, nil, err
	}
	mac, _ := netcfg.MAC(vm.IP)
	data, err = model.SerdeJSON(firecrackerConfiguration(vm, mac))
	if err != nil {
		return nil, nil, fmt.Errorf("Firecracker config: %w", err)
	}
	configPath := filepath.Join(s.Root, "config.json")
	if err := privateWrite(configPath, data); err != nil {
		return nil, nil, err
	}
	if err := f.chown(configPath, int(uid), int(uid)); err != nil {
		return nil, nil, fmt.Errorf("config ownership: %w", err)
	}
	if err := f.prepareTap(ctx, vm.ID, uid); err != nil {
		return nil, nil, err
	}
	uidText := strconv.FormatUint(uint64(uid), 10)
	cmd := exec.Command(jailer, "--id", id, "--exec-file", binary, "--uid", uidText, "--gid", uidText, "--chroot-base-dir", base, "--cgroup-version", "2", "--parent-cgroup", parentGroup, "--cgroup", fmt.Sprintf("cpu.max=%d 100000", uint64(vm.CPU)*100000), "--cgroup", fmt.Sprintf("memory.max=%d", (uint64(vm.Mem)+128)*1024*1024), "--cgroup", "memory.swap.max=0", "--cgroup", fmt.Sprintf("pids.max=%d", 64+uint64(vm.CPU)*2), "--", "--api-sock", s.Marker, "--config-file", "/config.json")
	return s, cmd, nil
}
