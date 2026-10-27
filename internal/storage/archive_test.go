package storage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aisidev/tt/model"
)

func TestPortableArchiveDigestAndMemberEnvelope(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "disk")
	data := bytes.Repeat([]byte("a"), 1<<20)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var archive bytes.Buffer
	manifest := model.DiskArchive{Version: 1, SizeMiB: 1, Format: "raw"}
	if err = WriteDiskArchive(context.Background(), &archive, manifest, []ArchiveInput{{Name: "disk.raw", File: file, Bytes: int64(len(data))}}); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(archive.Bytes()))
	stage := filepath.Join(root, "stage")
	got, err := ExtractDiskArchive(context.Background(), bytes.NewReader(archive.Bytes()), stage, digest, int64(archive.Len()), 2)
	if err != nil || got.SizeMiB != 1 {
		t.Fatal(got, err)
	}
	actual, err := os.ReadFile(filepath.Join(stage, "disk.raw"))
	if err != nil || !bytes.Equal(data, actual) {
		t.Fatal(err)
	}
	if _, err = ExtractDiskArchive(context.Background(), bytes.NewReader(archive.Bytes()), filepath.Join(root, "wrong"), fmt.Sprintf("%064d", 0), int64(archive.Len()), 2); err == nil {
		t.Fatal("digest mismatch published")
	}
	if _, err = ExtractDiskArchive(context.Background(), bytes.NewReader(archive.Bytes()), filepath.Join(root, "small"), digest, int64(archive.Len()-1), 2); err == nil {
		t.Fatal("archive exceeded compressed admission")
	}
}

func TestArchivePublicationPreservesStageAndRefusesExistingTargets(t *testing.T) {
	stage := t.TempDir()
	members := map[string][]byte{"vmlinux": []byte("verified-kernel"), "rootfs.ext4": bytes.Repeat([]byte("r"), 1<<20)}
	manifest := model.DiskArchive{Version: 1, Engine: model.Firecracker, Image: "fixture", Format: "raw", SizeMiB: 1}
	for _, name := range []string{"vmlinux", "rootfs.ext4"} {
		data := members[name]
		if err := os.WriteFile(filepath.Join(stage, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		manifest.Members = append(manifest.Members, model.ArchiveMember{Name: name, Bytes: int64(len(data))})
	}
	target := filepath.Join(t.TempDir(), "published")
	if err := PublishDiskArchive(context.Background(), FileStore{}, stage, target, manifest, false); err != nil {
		t.Fatal(err)
	}
	for name, expected := range members {
		for _, root := range []string{stage, target} {
			data, err := os.ReadFile(filepath.Join(root, name))
			if err != nil || !bytes.Equal(data, expected) {
				t.Fatal("publication lost verified source or destination bytes", root, name, err)
			}
		}
	}
	if err := PublishDiskArchive(context.Background(), FileStore{}, stage, target, manifest, false); err == nil {
		t.Fatal("publication adopted an existing target")
	}
	data, err := os.ReadFile(filepath.Join(target, "rootfs.ext4"))
	if err != nil || !bytes.Equal(data, members["rootfs.ext4"]) {
		t.Fatal("refused replay changed published bytes", err)
	}
}
func TestArchiveTraversalAndLinksRejected(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "disk.raw"} {
		var raw bytes.Buffer
		gzipWriter := gzip.NewWriter(&raw)
		archive := tar.NewWriter(gzipWriter)
		manifest := []byte(`{"version":1,"format":"raw","size_mib":1,"members":[{"name":"disk.raw","bytes":1048576}]}`)
		if err := archive.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(manifest))}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(manifest); err != nil {
			t.Fatal(err)
		}
		if err := archive.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}); err != nil {
			t.Fatal(err)
		}
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gzipWriter.Close(); err != nil {
			t.Fatal(err)
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(raw.Bytes()))
		if _, err := ExtractDiskArchive(context.Background(), bytes.NewReader(raw.Bytes()), filepath.Join(t.TempDir(), "stage"), digest, int64(raw.Len()), 2); err == nil {
			t.Fatal("unsafe archive accepted", name)
		}
	}
}

func TestArchiveRejectsRawLengthAndTrailingPayload(t *testing.T) {
	if err := ValidDiskArchive(model.DiskArchive{Version: 1, Format: "raw", SizeMiB: 1, Members: []model.ArchiveMember{{Name: "disk.raw", Bytes: 1}}}); err == nil {
		t.Fatal("short raw disk accepted")
	}
	file, err := os.CreateTemp(t.TempDir(), "disk")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err = file.Truncate(1 << 20); err != nil {
		t.Fatal(err)
	}
	var canonical bytes.Buffer
	if err = WriteDiskArchive(context.Background(), &canonical, model.DiskArchive{Version: 1, Format: "raw", SizeMiB: 1}, []ArchiveInput{{Name: "disk.raw", File: file, Bytes: 1 << 20}}); err != nil {
		t.Fatal(err)
	}
	appended := append(append([]byte{}, canonical.Bytes()...), []byte("trailing")...)
	hash := fmt.Sprintf("%x", sha256.Sum256(appended))
	if _, err = ExtractDiskArchive(context.Background(), bytes.NewReader(appended), filepath.Join(t.TempDir(), "stage"), hash, int64(len(appended)), 2); err == nil {
		t.Fatal("trailing compressed bytes accepted")
	}
	reader, err := gzip.NewReader(bytes.NewReader(canonical.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var unpacked bytes.Buffer
	if _, err = unpacked.ReadFrom(reader); err != nil {
		t.Fatal(err)
	}
	var altered bytes.Buffer
	gzipWriter := gzip.NewWriter(&altered)
	if _, err = gzipWriter.Write(append(unpacked.Bytes(), []byte("extra expanded data")...)); err != nil {
		t.Fatal(err)
	}
	if err = gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	hash = fmt.Sprintf("%x", sha256.Sum256(altered.Bytes()))
	if _, err = ExtractDiskArchive(context.Background(), bytes.NewReader(altered.Bytes()), filepath.Join(t.TempDir(), "stage"), hash, int64(altered.Len()), 2); err == nil {
		t.Fatal("trailing expanded bytes accepted")
	}
}
