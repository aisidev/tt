//go:build linux || freebsd

package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aisidev/tt/internal/backup"
	"github.com/aisidev/tt/internal/guestconfig"
	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
)

type BackupContext struct {
	VMID                 string
	Engine               model.Engine
	Backend              model.BackupBackend
	Disk, Root, LockPath string
	Dependencies         []string
	// OptionsDigest is serialized boot options, not a precomputed hash.
	OptionsDigest string
	ConfigDisk    bool
	Deadline      time.Time
}
type Witness struct {
	RootBytes                  uint64
	DiskIdentity, Dependencies string
}
type WorkerLock struct{ *os.File }

func (c BackupContext) Lock() (*WorkerLock, error) {
	file, err := runcmd.LockState(c.LockPath)
	if err != nil {
		return nil, fmt.Errorf("conflict: backup storage writer is still active; inspect or retry")
	}
	return &WorkerLock{file}, nil
}
func (c BackupContext) command(ctx context.Context, lock *WorkerLock, program string, args ...string) (string, error) {
	remaining := time.Until(c.Deadline)
	if remaining <= 0 {
		return "", fmt.Errorf("backup work budget exhausted; inspect and retry the same operation")
	}
	cmd := exec.Command(program, args...)
	cmd.ExtraFiles = []*os.File{lock.File}
	output, err := runcmd.Output(ctx, cmd, min(remaining, 60*time.Second))
	if err != nil {
		return "", err
	}
	if len(output.Stdout) >= runcmd.CaptureLimit || len(output.Stderr) >= runcmd.CaptureLimit {
		return "", fmt.Errorf("backup storage output reached its safety bound; inventory is unconfirmed")
	}
	if !output.Success() {
		text := []rune(strings.TrimSpace(strings.ToValidUTF8(string(output.Stderr), "\ufffd")))
		if len(text) > 4096 {
			text = text[:4096]
		}
		return "", fmt.Errorf("%s failed: %s", program, string(text))
	}
	if !utf8.Valid(output.Stdout) {
		return "", fmt.Errorf("invalid UTF-8 in storage output")
	}
	return strings.TrimSpace(string(output.Stdout)), nil
}
func (c BackupContext) property(ctx context.Context, lock *WorkerLock, object, name string) (string, error) {
	return c.command(ctx, lock, "zfs", "get", "-Hp", "-o", "value", name, object)
}
func (c BackupContext) numberProperty(ctx context.Context, lock *WorkerLock, object, name string) (uint64, error) {
	s, err := c.property(ctx, lock, object, name)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimPrefix(s, "+"), 10, 64)
}
func (c BackupContext) Inspect(ctx context.Context, lock *WorkerLock, probe bool) (Witness, error) {
	var witness Witness
	dependencies := append([]string(nil), c.Dependencies...)
	if c.Engine == model.Firecracker && c.Backend == model.BackupZvol {
		parent, ok := strings.CutSuffix(c.Disk, "/rootfs")
		if !ok {
			return witness, fmt.Errorf("invalid Firecracker volume")
		}
		directory, err := c.property(ctx, lock, parent, "mountpoint")
		if err != nil {
			return witness, err
		}
		if !filepath.IsAbs(directory) {
			return witness, fmt.Errorf("conflict: Firecracker kernel/config dataset must be mounted")
		}
		mounted, err := c.property(ctx, lock, parent, "mounted")
		if err != nil {
			return witness, err
		}
		if mounted != "yes" {
			return witness, fmt.Errorf("conflict: Firecracker kernel/config dataset must be mounted")
		}
		dependencies = append(dependencies, filepath.Join(directory, "vmlinux"))
		if c.ConfigDisk {
			dependencies = append(dependencies, filepath.Join(directory, guestconfig.ConfigDisk))
		}
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(c.OptionsDigest))
	for _, path := range dependencies {
		file, err := regular(path)
		if err != nil {
			return witness, err
		}
		name := filepath.Base(path)
		if name == "." || name == string(filepath.Separator) {
			file.Close()
			return witness, fmt.Errorf("invalid dependency path")
		}
		_, _ = hash.Write([]byte(name))
		_, copyErr := io.CopyBuffer(hash, file, make([]byte, 16384))
		file.Close()
		if copyErr != nil {
			return witness, copyErr
		}
	}
	witness.Dependencies = fmt.Sprintf("%x", hash.Sum(nil))
	switch c.Backend {
	case model.BackupZvol:
		kind, err := c.property(ctx, lock, c.Disk, "type")
		if err != nil {
			return witness, err
		}
		if kind != "volume" {
			return witness, fmt.Errorf("backup unsupported: root disk is not a zvol")
		}
		witness.RootBytes, err = c.numberProperty(ctx, lock, c.Disk, "volsize")
		if err != nil {
			return witness, err
		}
		witness.DiskIdentity, err = c.property(ctx, lock, c.Disk, "guid")
		if err != nil {
			return witness, err
		}
		if probe {
			readOnly, err := c.property(ctx, lock, c.Disk, "readonly")
			if err != nil {
				return witness, err
			}
			if readOnly != "off" {
				return witness, fmt.Errorf("backup unsupported: zvol must be writable with synchronous writes enabled")
			}
			syncMode, err := c.property(ctx, lock, c.Disk, "sync")
			if err != nil {
				return witness, err
			}
			if syncMode == "disabled" {
				return witness, fmt.Errorf("backup unsupported: zvol must be writable with synchronous writes enabled")
			}
			available, err := c.numberProperty(ctx, lock, c.Disk, "available")
			if err != nil {
				return witness, err
			}
			if available < 16*1024*1024 {
				return witness, fmt.Errorf("conflict: insufficient storage headroom for backup")
			}
		}
	case model.BackupReflink:
		file, err := regular(c.Disk)
		if err != nil {
			return witness, err
		}
		defer file.Close()
		meta, err := file.Stat()
		if err != nil {
			return witness, err
		}
		rootMeta, err := os.Lstat(c.Root)
		if err != nil {
			return witness, err
		}
		if !rootMeta.IsDir() || deviceID(rootMeta) != deviceID(meta) {
			return witness, fmt.Errorf("backup unsupported: disk and backup location require the same filesystem")
		}
		witness.RootBytes = uint64(meta.Size())
		if c.Engine == model.Qemu {
			data, err := c.command(ctx, lock, "qemu-img", "info", "--output=json", c.Disk)
			if err != nil {
				return witness, err
			}
			witness.RootBytes, err = qemuCapacity([]byte(data))
			if err != nil {
				return witness, err
			}
		}
		if probe {
			available, err := availableBytes(c.Root)
			if err != nil {
				return witness, err
			}
			if available < 16*1024*1024 {
				return witness, fmt.Errorf("conflict: insufficient storage headroom for backup")
			}
			if err := ProbeReflink(c.Root); err != nil {
				return witness, err
			}
		}
		witness.DiskIdentity, err = fileIdentity(file)
		if err != nil {
			return witness, err
		}
	default:
		return witness, fmt.Errorf("backup unsupported: unknown backend")
	}
	if witness.RootBytes == 0 {
		return witness, fmt.Errorf("invalid empty root disk")
	}
	return witness, nil
}
func (c BackupContext) directory() (string, error) {
	if err := model.ValidateName(c.VMID, "vm_id"); err != nil {
		return "", err
	}
	root := filepath.Join(c.Root, ".tt-backups")
	if err := privateDir(root); err != nil {
		return "", err
	}
	path := filepath.Join(root, c.VMID)
	if err := privateDir(path); err != nil {
		return "", err
	}
	return path, nil
}
func (c BackupContext) artifactPath(id string) (string, error) {
	if !backup.ValidToken(id) {
		return "", fmt.Errorf("invalid backup generation")
	}
	dir, err := c.directory()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id), nil
}
func (c BackupContext) snapshot(g model.BackupGeneration) (string, error) {
	if !backup.ValidToken(g.ID) {
		return "", fmt.Errorf("invalid backup generation")
	}
	return c.Disk + "@ttbackup-" + g.ID, nil
}

type snapshot struct{ Name, ID, Owner string }

func (c BackupContext) snapshots(ctx context.Context, lock *WorkerLock) ([]snapshot, error) {
	text, err := c.command(ctx, lock, "zfs", "list", "-Hpr", "-d", "1", "-t", "snapshot", "-o", "name,guid,ttstack:backup", "-s", "createtxg", c.Disk)
	if err != nil {
		return nil, err
	}
	result := []snapshot{}
	if text == "" {
		return result, nil
	}
	for _, line := range strings.Split(text, "\n") {
		values := strings.Split(strings.TrimSuffix(line, "\r"), "\t")
		if len(values) != 3 {
			return nil, fmt.Errorf("invalid ZFS snapshot listing")
		}
		result = append(result, snapshot{values[0], values[1], values[2]})
	}
	return result, nil
}
func (c BackupContext) Verify(ctx context.Context, lock *WorkerLock, g model.BackupGeneration) (model.BackupGeneration, error) {
	result := g
	switch g.Backend {
	case model.BackupZvol:
		name, err := c.snapshot(g)
		if err != nil {
			return result, err
		}
		entries, err := c.snapshots(ctx, lock)
		if err != nil {
			return result, err
		}
		found := false
		for _, s := range entries {
			if s.Name == name {
				found = true
				if s.Owner != c.VMID+":"+g.ID {
					return result, fmt.Errorf("conflict: backup snapshot ownership changed")
				}
				result.Identity = s.ID
				break
			}
		}
		if !found {
			return result, fmt.Errorf("not found: backup snapshot is missing")
		}
	case model.BackupReflink:
		dir, err := c.artifactPath(g.ID)
		if err != nil {
			return result, err
		}
		if err := c.checkOwner(dir, g.ID); err != nil {
			return result, err
		}
		manifest, err := readManifest(filepath.Join(dir, "ready.json"))
		if err != nil {
			return result, err
		}
		file, err := regular(filepath.Join(dir, "disk"))
		if err != nil {
			return result, err
		}
		id, err := fileIdentity(file)
		file.Close()
		if err != nil {
			return result, err
		}
		if manifest.VMID != c.VMID || manifest.Generation.ID != g.ID || manifest.Generation.RootBytes != g.RootBytes || manifest.Generation.Dependencies != g.Dependencies || manifest.Generation.Identity != id {
			return result, fmt.Errorf("conflict: backup file identity or manifest changed")
		}
		result.Identity = manifest.Generation.Identity
	default:
		return result, fmt.Errorf("backup unsupported: unknown backend")
	}
	if g.Identity != "" && g.Identity != result.Identity {
		return result, fmt.Errorf("conflict: backup artifact was replaced")
	}
	return result, nil
}
func (c BackupContext) checkOwner(dir, id string) error {
	meta, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !meta.IsDir() {
		return fmt.Errorf("conflict: backup directory is not a directory")
	}
	owner, err := regular(filepath.Join(dir, "owner"))
	if err != nil {
		return err
	}
	data, err := io.ReadAll(owner)
	owner.Close()
	if err != nil {
		return err
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("invalid UTF-8 in backup owner")
	}
	if string(data) != c.VMID+":"+id {
		return fmt.Errorf("conflict: backup directory ownership changed")
	}
	return nil
}
func emptyPrivateDir(path string) (bool, error) {
	meta, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !meta.IsDir() || meta.Mode().Perm()&0077 != 0 {
		return false, nil
	}
	entries, err := os.ReadDir(path)
	return len(entries) == 0, err
}
func (c BackupContext) prepareDirectory(id string) (string, error) {
	dir, err := c.artifactPath(id)
	if err != nil {
		return "", err
	}
	err = os.Mkdir(dir, 0700)
	switch {
	case err == nil:
		if err := atomicWrite(filepath.Join(dir, "owner"), []byte(c.VMID+":"+id)); err != nil {
			return "", err
		}
		if err := syncDirectory(filepath.Dir(dir)); err != nil {
			return "", err
		}
	case os.IsExist(err):
		empty, err := emptyPrivateDir(dir)
		if err != nil {
			return "", err
		}
		if empty {
			if err := atomicWrite(filepath.Join(dir, "owner"), []byte(c.VMID+":"+id)); err != nil {
				return "", err
			}
		} else if err := c.checkOwner(dir, id); err != nil {
			return "", err
		}
	default:
		return "", err
	}
	return dir, nil
}
func (c BackupContext) Create(ctx context.Context, lock *WorkerLock, p model.BackupPending) (model.BackupGeneration, error) {
	g := p.Artifact
	witness, err := c.Inspect(ctx, lock, false)
	if err != nil {
		return g, err
	}
	if err := checkWitness(p, witness); err != nil {
		return g, err
	}
	if c.Backend == model.BackupZvol {
		name, err := c.snapshot(g)
		if err != nil {
			return g, err
		}
		entries, err := c.snapshots(ctx, lock)
		if err != nil {
			return g, err
		}
		exists := false
		for _, s := range entries {
			exists = exists || s.Name == name
		}
		if !exists {
			if _, err := c.command(ctx, lock, "zfs", "snapshot", "-o", "ttstack:backup="+c.VMID+":"+g.ID, name); err != nil {
				return g, err
			}
		}
		return c.Verify(ctx, lock, g)
	}
	dir, err := c.prepareDirectory(g.ID)
	if err != nil {
		return g, err
	}
	exists, err := tryExists(filepath.Join(dir, "ready.json"))
	if err != nil {
		return g, err
	}
	if exists {
		return c.Verify(ctx, lock, g)
	}
	diskPath := filepath.Join(dir, "disk")
	if err := removeRegular(diskPath); err != nil {
		return g, err
	}
	source, err := regular(c.Disk)
	if err != nil {
		return g, err
	}
	defer source.Close()
	if err := source.Sync(); err != nil {
		return g, err
	}
	destination, err := newFile(diskPath)
	if err != nil {
		return g, err
	}
	defer destination.Close()
	if err := reflink(source, destination); err != nil {
		return g, err
	}
	if err := destination.Sync(); err != nil {
		return g, err
	}
	g.Identity, err = fileIdentity(destination)
	if err != nil {
		return g, err
	}
	if err := writeManifest(filepath.Join(dir, "ready.json"), manifest{c.VMID, g}); err != nil {
		return g, err
	}
	return c.Verify(ctx, lock, g)
}
func (c BackupContext) Restore(ctx context.Context, lock *WorkerLock, p model.BackupPending, source model.BackupGeneration) (*model.BackupGeneration, error) {
	if _, err := c.Verify(ctx, lock, source); err != nil {
		return nil, err
	}
	witness, err := c.Inspect(ctx, lock, false)
	if err != nil {
		return nil, err
	}
	if witness.RootBytes != source.RootBytes || witness.Dependencies != source.Dependencies {
		return nil, fmt.Errorf("conflict: backup capacity or retained boot dependencies changed")
	}
	if c.Backend == model.BackupZvol {
		if err := checkWitness(p, witness); err != nil {
			return nil, err
		}
		name, err := c.snapshot(source)
		if err != nil {
			return nil, err
		}
		_, err = c.command(ctx, lock, "zfs", "rollback", name)
		return nil, err
	}
	stage, err := c.prepareDirectory(p.Artifact.ID)
	if err != nil {
		return nil, err
	}
	replacement := p.Artifact
	exists, err := tryExists(filepath.Join(stage, "ready.json"))
	if err != nil {
		return nil, err
	}
	if exists {
		saved, err := readManifest(filepath.Join(stage, "ready.json"))
		if err != nil {
			return nil, err
		}
		if saved.VMID != c.VMID || saved.Generation.ID != replacement.ID {
			return nil, fmt.Errorf("conflict: restore staging identity changed")
		}
		replacement = saved.Generation
		if witness.DiskIdentity == replacement.Identity {
			file, err := regular(c.Disk)
			if err != nil {
				return nil, err
			}
			err = file.Sync()
			file.Close()
			if err != nil {
				return nil, err
			}
			if err := syncDirectory(filepath.Dir(c.Disk)); err != nil {
				return nil, err
			}
			return &replacement, nil
		}
	}
	if err := checkWitness(p, witness); err != nil {
		return nil, err
	}
	stagedDisk := filepath.Join(stage, "disk")
	if replacement.Identity == "" {
		if err := removeRegular(stagedDisk); err != nil {
			return nil, err
		}
		original, err := c.artifactPath(source.ID)
		if err != nil {
			return nil, err
		}
		input, err := regular(filepath.Join(original, "disk"))
		if err != nil {
			return nil, err
		}
		defer input.Close()
		output, err := newFile(stagedDisk)
		if err != nil {
			return nil, err
		}
		defer output.Close()
		if err := reflink(input, output); err != nil {
			return nil, err
		}
		if err := output.Sync(); err != nil {
			return nil, err
		}
		replacement.Identity, err = fileIdentity(output)
		if err != nil {
			return nil, err
		}
		if err := writeManifest(filepath.Join(stage, "ready.json"), manifest{c.VMID, replacement}); err != nil {
			return nil, err
		}
	}
	file, err := regular(stagedDisk)
	if err != nil {
		return nil, err
	}
	id, err := fileIdentity(file)
	file.Close()
	if err != nil {
		return nil, err
	}
	if id != replacement.Identity {
		return nil, fmt.Errorf("conflict: restore file was replaced")
	}
	if err := os.Rename(stagedDisk, c.Disk); err != nil {
		return nil, err
	}
	if err := syncDirectory(filepath.Dir(c.Disk)); err != nil {
		return nil, err
	}
	if err := syncDirectory(stage); err != nil {
		return nil, err
	}
	return &replacement, nil
}
func (c BackupContext) PrepareRestore(ctx context.Context, lock *WorkerLock, source model.BackupGeneration, retired []model.BackupGeneration) ([]string, error) {
	removed := []string{}
	if c.Backend != model.BackupZvol {
		return removed, nil
	}
	entries, err := c.snapshots(ctx, lock)
	if err != nil {
		return nil, err
	}
	name, err := c.snapshot(source)
	if err != nil {
		return nil, err
	}
	index := -1
	for i, s := range entries {
		if s.Name == name {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, fmt.Errorf("not found: backup snapshot missing")
	}
	for _, s := range entries[index+1:] {
		var found *model.BackupGeneration
		for i := range retired {
			if n, err := c.snapshot(retired[i]); err == nil && n == s.Name {
				found = &retired[i]
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("conflict: newer foreign snapshot prevents backup restore")
		}
		if err := c.Remove(ctx, lock, *found); err != nil {
			return nil, err
		}
		removed = append(removed, found.ID)
	}
	return removed, nil
}
func (c BackupContext) Remove(ctx context.Context, lock *WorkerLock, g model.BackupGeneration) error {
	if g.Backend == model.BackupZvol {
		name, err := c.snapshot(g)
		if err != nil {
			return err
		}
		entries, err := c.snapshots(ctx, lock)
		if err != nil {
			return err
		}
		for _, s := range entries {
			if s.Name == name {
				if _, err := c.Verify(ctx, lock, g); err != nil {
					return err
				}
				_, err := c.command(ctx, lock, "zfs", "destroy", name)
				return err
			}
		}
		return nil
	}
	dir, err := c.artifactPath(g.ID)
	if err != nil {
		return err
	}
	exists, err := tryExists(dir)
	if err != nil || !exists {
		return err
	}
	empty, err := emptyPrivateDir(dir)
	if err != nil {
		return err
	}
	if empty {
		if err := os.Remove(dir); err != nil {
			return err
		}
		return syncDirectory(filepath.Dir(dir))
	}
	if err := c.checkOwner(dir, g.ID); err != nil {
		return err
	}
	disk := filepath.Join(dir, "disk")
	exists, err = tryExists(disk)
	if err != nil {
		return err
	}
	if exists {
		if g.Identity != "" {
			if _, err := c.Verify(ctx, lock, g); err != nil {
				return err
			}
		}
		if err := removeRegular(disk); err != nil {
			return err
		}
	}
	if err := removeRegular(filepath.Join(dir, "ready.json")); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) != 1 {
		return fmt.Errorf("conflict: unexpected backup directory contents")
	}
	if err := removeRegular(filepath.Join(dir, "owner")); err != nil {
		return err
	}
	if err := os.Remove(dir); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(dir))
}
func checkWitness(p model.BackupPending, w Witness) error {
	if w.DiskIdentity != p.DiskIdentity || w.RootBytes != p.Artifact.RootBytes || w.Dependencies != p.Artifact.Dependencies {
		return fmt.Errorf("conflict: disk identity, capacity or boot dependencies changed")
	}
	return nil
}

type manifest struct {
	VMID       string                 `json:"vm_id"`
	Generation model.BackupGeneration `json:"generation"`
}

func (m *manifest) UnmarshalJSON(data []byte) error {
	type plain manifest
	var next plain
	if err := model.DecodeRecord(data, &next, []string{"vm_id", "generation"}, false); err != nil {
		return err
	}
	*m = manifest(next)
	return nil
}
func readManifest(path string) (manifest, error) {
	var m manifest
	file, err := regular(path)
	if err != nil {
		return m, err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(data, &m)
	return m, err
}
func writeManifest(path string, m manifest) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}
func qemuCapacity(data []byte) (uint64, error) {
	var info map[string]json.RawMessage
	if err := json.Unmarshal(data, &info); err != nil {
		return 0, err
	}
	var specific struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(info["format-specific"], &specific)
	var format string
	_ = json.Unmarshal(info["format"], &format)
	truth := func(raw json.RawMessage) bool { var value bool; _ = json.Unmarshal(raw, &value); return value }
	object := func(raw json.RawMessage) bool { return strings.HasPrefix(strings.TrimSpace(string(raw)), "{") }
	_, backing := info["backing-filename"]
	_, external := specific.Data["data-file"]
	var snapshots []json.RawMessage
	_ = json.Unmarshal(info["snapshots"], &snapshots)
	if format != "qcow2" || backing || truth(info["encrypted"]) || truth(info["dirty-flag"]) || truth(specific.Data["corrupt"]) || object(specific.Data["encrypt"]) || external || len(snapshots) > 0 {
		return 0, fmt.Errorf("backup unsupported: QEMU requires a clean standalone qcow2 without snapshots, encryption or external data")
	}
	var size uint64
	if raw, ok := info["virtual-size"]; !ok || string(raw) == "null" || json.Unmarshal(raw, &size) != nil {
		return 0, fmt.Errorf("invalid QEMU disk capacity")
	}
	return size, nil
}
func tryExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}
func privateDir(path string) error {
	err := os.Mkdir(path, 0700)
	if err == nil {
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return err
		}
	} else if !os.IsExist(err) {
		return err
	}
	meta, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !meta.IsDir() || meta.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("conflict: backup directory must be private and not a symbolic link")
	}
	return nil
}
func atomicWrite(path string, data []byte) error {
	parent := filepath.Dir(path)
	file, err := os.CreateTemp(parent, ".backup-*")
	if err != nil {
		return err
	}
	defer file.Close()
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	return syncDirectory(parent)
}
func removeRegular(path string) error {
	meta, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !meta.Mode().IsRegular() {
		return fmt.Errorf("conflict: refusing to remove non-regular backup file")
	}
	return os.Remove(path)
}
func ProbeReflink(root string) error {
	source, err := runcmd.AnonymousFile(root, ".backup-probe-*")
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := runcmd.AnonymousFile(root, ".backup-probe-*")
	if err != nil {
		return err
	}
	defer target.Close()
	if _, err := source.WriteString("backup-probe"); err != nil {
		return err
	}
	if err := reflink(source, target); err != nil {
		return err
	}
	if _, err := target.WriteString("changed"); err != nil {
		return err
	}
	if _, err := source.Seek(0, 0); err != nil {
		return err
	}
	data, err := io.ReadAll(source)
	if err != nil {
		return err
	}
	if string(data) != "backup-probe" {
		return fmt.Errorf("backup unsupported: cloned data is not independent")
	}
	return nil
}
