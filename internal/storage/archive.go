package storage

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
)

const ArchiveManifestLimit = 16 << 10
const ArchiveOverheadMiB = 80

// ArchiveInput is an opened descriptor paired with a fixed-format member name.
// Export callers open descriptors under ownership exclusion, then release global
// lifecycle locks before streaming on their bounded transfer lane.
type ArchiveInput struct {
	Name  string
	File  *os.File
	Bytes int64
}

func ValidDiskArchive(manifest model.DiskArchive) error {
	if manifest.Version != 1 || manifest.SizeMiB == 0 || manifest.SizeMiB > 1<<20 || len(manifest.Members) < 1 || len(manifest.Members) > 3 {
		return fmt.Errorf("invalid disk archive envelope")
	}
	if (manifest.Engine == "" && manifest.Image != "") || (manifest.Engine != "" && model.ValidateImage(manifest.Image, manifest.Engine) != nil) {
		return fmt.Errorf("invalid archive source image")
	}
	expected := map[string]int64{}
	switch manifest.Engine {
	case model.Firecracker:
		if manifest.Format != "raw" {
			return fmt.Errorf("invalid Firecracker archive format")
		}
		expected["vmlinux"] = 64 << 20
		expected["rootfs.ext4"] = int64(manifest.SizeMiB) << 20
		expected["guest-config.ext4"] = int64(model.ConfigDiskMiB) << 20
	case model.Qemu:
		if manifest.Format == "qcow2" {
			expected["disk.qcow2"] = int64(manifest.SizeMiB+manifest.SizeMiB/128+8) << 20
		} else if manifest.Format == "raw" {
			expected["disk.raw"] = int64(manifest.SizeMiB) << 20
		} else {
			return fmt.Errorf("invalid QEMU archive format")
		}
	case "":
		if manifest.Format != "raw" {
			return fmt.Errorf("invalid data archive format")
		}
		expected["disk.raw"] = int64(manifest.SizeMiB) << 20
	default:
		return fmt.Errorf("unsupported archive engine")
	}
	seen := map[string]bool{}
	for _, member := range manifest.Members {
		limit, ok := expected[member.Name]
		if !ok || seen[member.Name] || member.Bytes < 1 || member.Bytes > limit {
			return fmt.Errorf("invalid archive member")
		}
		if member.Name != "vmlinux" && member.Name != "disk.qcow2" && member.Bytes != limit {
			return fmt.Errorf("raw archive member has a different disk length")
		}
		seen[member.Name] = true
	}
	if manifest.Engine == model.Firecracker {
		if !seen["vmlinux"] || !seen["rootfs.ext4"] {
			return fmt.Errorf("missing root archive members")
		}
	} else if len(manifest.Members) != 1 {
		return fmt.Errorf("disk archive must contain one disk")
	}
	return nil
}
func WriteDiskArchive(ctx context.Context, dst io.Writer, manifest model.DiskArchive, inputs []ArchiveInput) error {
	manifest.Members = []model.ArchiveMember{}
	for _, input := range inputs {
		manifest.Members = append(manifest.Members, model.ArchiveMember{Name: input.Name, Bytes: input.Bytes})
	}
	if err := ValidDiskArchive(manifest); err != nil {
		return err
	}
	compressed, err := gzip.NewWriterLevel(dst, gzip.BestSpeed)
	if err != nil {
		return err
	}
	archive := tar.NewWriter(compressed)
	raw, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if len(raw) > ArchiveManifestLimit {
		return fmt.Errorf("archive manifest exceeds budget")
	}
	if err = archive.WriteHeader(&tar.Header{Format: tar.FormatUSTAR, Name: "manifest.json", Mode: 0600, Size: int64(len(raw))}); err != nil {
		return err
	}
	if _, err = archive.Write(raw); err != nil {
		return err
	}
	for _, input := range inputs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err = input.File.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err = archive.WriteHeader(&tar.Header{Format: tar.FormatUSTAR, Name: input.Name, Mode: 0600, Size: input.Bytes}); err != nil {
			return err
		}
		if _, err = io.CopyN(archive, contextReader{ctx, input.File}, input.Bytes); err != nil {
			return err
		}
	}
	if err = archive.Close(); err != nil {
		return err
	}
	return compressed.Close()
}

type contextReader struct {
	ctx context.Context
	src io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if r.ctx.Err() != nil {
		return 0, r.ctx.Err()
	}
	return r.src.Read(b)
}

// ExtractDiskArchive writes only into a caller-owned private unpublished stage.
// It rejects links, directories, traversal, undeclared members, length overflow,
// expansion beyond the declared envelope and digest mismatches before publication.
func ExtractDiskArchive(ctx context.Context, src io.Reader, stage string, expectedDigest string, maxCompressed int64, maxExpandedMiB uint32) (model.DiskArchive, error) {
	var manifest model.DiskArchive
	if len(expectedDigest) != 64 || maxCompressed < 1 || maxExpandedMiB < 1 || maxExpandedMiB > (1<<20)+ArchiveOverheadMiB {
		return manifest, fmt.Errorf("invalid archive admission")
	}
	if _, err := hex.DecodeString(expectedDigest); err != nil {
		return manifest, err
	}
	if err := os.Mkdir(stage, 0700); err != nil {
		return manifest, err
	}
	hash := sha256.New()
	limited := &io.LimitedReader{R: contextReader{ctx, src}, N: maxCompressed + 1}
	tee := io.TeeReader(limited, hash)
	buffered := bufio.NewReader(tee)
	compressed, err := gzip.NewReader(buffered)
	if err != nil {
		return manifest, err
	}
	compressed.Multistream(false)
	defer compressed.Close()
	expandedStream := &io.LimitedReader{R: compressed, N: (int64(maxExpandedMiB) << 20) + ArchiveManifestLimit + 5*512 + 1}
	archive := tar.NewReader(expandedStream)
	header, err := archive.Next()
	if err != nil {
		return manifest, err
	}
	if header.Name != "manifest.json" || header.Typeflag != tar.TypeReg || header.Format != tar.FormatUSTAR || header.Size < 1 || header.Size > ArchiveManifestLimit {
		return manifest, fmt.Errorf("invalid archive manifest")
	}
	raw, err := io.ReadAll(io.LimitReader(archive, ArchiveManifestLimit+1))
	if err != nil {
		return manifest, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&manifest); err != nil {
		return manifest, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return manifest, fmt.Errorf("archive manifest has trailing JSON")
	}
	if err = ValidDiskArchive(manifest); err != nil {
		return manifest, err
	}
	var expanded int64
	for _, member := range manifest.Members {
		expanded += member.Bytes
	}
	if expanded > int64(maxExpandedMiB)<<20 {
		return manifest, fmt.Errorf("archive expansion exceeds admitted budget")
	}
	for _, member := range manifest.Members {
		header, err = archive.Next()
		if err != nil {
			return manifest, err
		}
		if header.Typeflag != tar.TypeReg || header.Format != tar.FormatUSTAR || header.Name != member.Name || header.Size != member.Bytes {
			return manifest, fmt.Errorf("undeclared or malformed archive member")
		}
		file, err := os.OpenFile(filepath.Join(stage, member.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return manifest, err
		}
		_, copyErr := io.CopyN(file, contextReader{ctx, archive}, member.Bytes)
		syncErr := file.Sync()
		closeErr := file.Close()
		if copyErr != nil {
			return manifest, copyErr
		}
		if syncErr != nil {
			return manifest, syncErr
		}
		if closeErr != nil {
			return manifest, closeErr
		}
	}
	if _, err = archive.Next(); err != io.EOF {
		return manifest, fmt.Errorf("archive contains extra members")
	}
	// The canonical tar writer ends with two zero blocks. Nothing may follow
	// those blocks, including another gzip stream or an expansion bomb.
	var tail [1]byte
	if n, endErr := compressed.Read(tail[:]); n != 0 || endErr != io.EOF {
		return manifest, fmt.Errorf("archive has trailing expanded data")
	}
	if _, endErr := buffered.ReadByte(); endErr != io.EOF {
		return manifest, fmt.Errorf("archive has trailing compressed data")
	}
	if limited.N != 1 {
		return manifest, fmt.Errorf("compressed archive differs from declared length")
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
		return manifest, fmt.Errorf("archive digest mismatch")
	}
	if err = validateArchiveDisk(ctx, stage, manifest); err != nil {
		return manifest, err
	}
	return manifest, syncDirectory(stage)
}

// PublishDiskArchive copies only verified members into a caller-owned new target.
// It never adopts an existing target. The caller's durable intent owns rollback
// of an interrupted unpublished target and publishes readiness only after sync.
func PublishDiskArchive(ctx context.Context, store ImageStore, stage, target string, manifest model.DiskArchive, image bool) error {
	if err := ValidDiskArchive(manifest); err != nil {
		return err
	}
	if store.Name() == "file" {
		if manifest.Engine == model.Firecracker || image {
			if err := os.Mkdir(target, 0700); err != nil {
				return err
			}
			if manifest.Engine == model.Qemu {
				if err := publishQemuArchive(ctx, stage, filepath.Join(target, "disk.qcow2"), manifest); err != nil {
					return err
				}
			} else {
				for _, member := range manifest.Members {
					// The journal's private stage and runtime/image storage can
					// reside on different filesystems. Publish exclusive copies;
					// the caller removes its stage only after the full sync.
					if err := copyArchiveMember(ctx, filepath.Join(stage, member.Name), filepath.Join(target, member.Name), false); err != nil {
						return err
					}
				}
			}
			return SyncVolume(ctx, store, target, manifest.Engine)
		}
		if manifest.Engine == model.Qemu {
			if err := publishQemuArchive(ctx, stage, target, manifest); err != nil {
				return err
			}
		} else if err := copyArchiveMember(ctx, filepath.Join(stage, manifest.Members[0].Name), target, false); err != nil {
			return err
		}
		return SyncVolume(ctx, store, target, manifest.Engine)
	}

	if store.Name() != "zvol" {
		return fmt.Errorf("archive backend unsupported")
	}
	if manifest.Engine == model.Firecracker {
		index := strings.LastIndexByte(target, '/')
		if index < 0 {
			return fmt.Errorf("invalid target dataset")
		}
		parent, err := zfsMountpoint(ctx, target[:index])
		if err != nil {
			return err
		}
		mount := filepath.Join(parent, target[index+1:])
		if _, err = zfsCommand(ctx, "create", "-o", "mountpoint="+mount, target); err != nil {
			return err
		}
		if err = CreateRawVolume(ctx, store, target+"/rootfs", manifest.SizeMiB); err != nil {
			return err
		}
		device, err := ResolveVolumeDevice(ctx, store, target+"/rootfs")
		if err != nil {
			return err
		}
		for _, member := range manifest.Members {
			dest := filepath.Join(mount, member.Name)
			if member.Name == "rootfs.ext4" {
				dest = device
			}
			if err = copyArchiveMember(ctx, filepath.Join(stage, member.Name), dest, member.Name == "rootfs.ext4"); err != nil {
				return err
			}
		}
		zvol := store.(ZvolStore)
		if err = rewireZvolRoot(ctx, zvol, target, mount); err != nil {
			return err
		}
	} else {
		if err := CreateRawVolume(ctx, store, target, manifest.SizeMiB); err != nil {
			return err
		}
		device, err := ResolveVolumeDevice(ctx, store, target)
		if err != nil {
			return err
		}
		source := filepath.Join(stage, manifest.Members[0].Name)
		if manifest.Format == "qcow2" {
			result, err := runcmd.Run(ctx, "qemu-img", "convert", "-n", "-f", "qcow2", "-O", "raw", source, device)
			if err != nil {
				return err
			}
			if !result.Success() {
				return fmt.Errorf("archive disk conversion failed")
			}
		} else if err = copyArchiveMember(ctx, source, device, true); err != nil {
			return err
		}
	}
	if image {
		if _, err := zfsCommand(ctx, "snapshot", "-r", target+"@"+CloneSnapshot); err != nil {
			return err
		}
	}
	return SyncVolume(ctx, store, target, manifest.Engine)
}
func copyArchiveMember(ctx context.Context, source, target string, existingDevice bool) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if existingDevice {
		flags = os.O_WRONLY
	}
	output, err := os.OpenFile(target, flags, 0600)
	if err != nil {
		return err
	}
	defer output.Close()
	if _, err = io.Copy(output, contextReader{ctx, input}); err != nil {
		return err
	}
	return output.Sync()
}

// A qcow2 member may never resolve backing files or external data on this host.
// The virtual size is part of admission, not an attacker-controlled expansion.
func validateArchiveDisk(ctx context.Context, stage string, manifest model.DiskArchive) error {
	if manifest.Format != "qcow2" {
		return nil
	}
	path := filepath.Join(stage, "disk.qcow2")
	out, err := runcmd.Run(ctx, "qemu-img", "info", "--output=json", "-f", "qcow2", path)
	if err != nil || !out.Success() {
		return fmt.Errorf("cannot inspect archive disk")
	}
	var info struct {
		Size           int64  `json:"virtual-size"`
		Backing        string `json:"backing-filename"`
		FullBacking    string `json:"full-backing-filename"`
		Encrypted      bool   `json:"encrypted"`
		FormatSpecific struct {
			Data struct {
				DataFile string `json:"data-file"`
			} `json:"data"`
		} `json:"format-specific"`
	}
	if json.Unmarshal(out.Stdout, &info) != nil || info.Size != int64(manifest.SizeMiB)<<20 || info.Backing != "" || info.FullBacking != "" || info.Encrypted || info.FormatSpecific.Data.DataFile != "" {
		return fmt.Errorf("archive disk is not a standalone admitted image")
	}
	out, err = runcmd.Run(ctx, "qemu-img", "check", "-f", "qcow2", path)
	if err != nil || !out.Success() {
		return fmt.Errorf("archive disk integrity check failed")
	}
	return nil
}
func publishQemuArchive(ctx context.Context, stage, target string, manifest model.DiskArchive) error {
	source := filepath.Join(stage, manifest.Members[0].Name)
	if manifest.Format == "qcow2" {
		return copyArchiveMember(ctx, source, target, false)
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	out, err := runcmd.Run(ctx, "qemu-img", "convert", "-f", "raw", "-O", "qcow2", source, target)
	if err != nil || !out.Success() {
		return fmt.Errorf("archive disk conversion failed")
	}
	return nil
}
