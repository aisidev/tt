//go:build linux || freebsd

package storage

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

func testBackupContext(root string) BackupContext {
	return BackupContext{VMID: "owned-vm", Engine: model.Firecracker, Backend: model.BackupReflink, Disk: filepath.Join(root, "rootfs.ext4"), Root: root, LockPath: filepath.Join(root, "worker.lock"), Deadline: time.Now().Add(10 * time.Second)}
}
func testGeneration() model.BackupGeneration {
	return model.BackupGeneration{ID: uuid.NewString(), Backend: model.BackupReflink, RootBytes: 4096, CreatedAt: 1}
}
func TestQCOW2CorruptionAndExternalDependenciesAreNotPublishableBackups(t *testing.T) {
	clean := map[string]any{"format": "qcow2", "virtual-size": uint64(1048576), "format-specific": map[string]any{"data": map[string]any{"corrupt": false}}}
	data, _ := json.Marshal(clean)
	if size, err := qemuCapacity(data); err != nil || size != 1048576 {
		t.Fatal(size, err)
	}
	for key, value := range map[string]any{"corrupt": true, "encrypt": map[string]any{"format": "luks"}, "data-file": "external.raw"} {
		clean["format-specific"] = map[string]any{"data": map[string]any{key: value}}
		data, _ := json.Marshal(clean)
		if _, err := qemuCapacity(data); err == nil || !strings.HasPrefix(err.Error(), "backup unsupported:") {
			t.Fatal(key, err)
		}
	}
}
func TestBackupCleanupRejectsForeignFilesAndRecoversEmptyTombstone(t *testing.T) {
	c := testBackupContext(t.TempDir())
	lock, err := c.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	g := testGeneration()
	dir, err := c.prepareDirectory(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "foreign")
	writeTestFile(t, foreign, []byte("keep"))
	if err := c.Remove(context.Background(), lock, g); err == nil {
		t.Fatal("foreign contents accepted")
	}
	data, err := os.ReadFile(foreign)
	if err != nil || string(data) != "keep" {
		t.Fatal("foreign file removed", err)
	}
	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "owner")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := c.Remove(context.Background(), lock, g); err != nil {
			t.Fatal(err)
		}
	}
	elsewhere := filepath.Join(c.Root, "foreign-dir")
	if err := os.Mkdir(elsewhere, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(context.Background(), lock, g); err == nil {
		t.Fatal("symlink accepted")
	}
	if info, err := os.Stat(elsewhere); err != nil || !info.IsDir() {
		t.Fatal("foreign directory removed", err)
	}
}
func TestBackupRequiresRegularDependenciesAndStableWitness(t *testing.T) {
	c := testBackupContext(t.TempDir())
	writeTestFile(t, c.Disk, make([]byte, 4096))
	lock, err := c.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	witness, err := c.Inspect(context.Background(), lock, false)
	if err != nil {
		t.Fatal(err)
	}
	p := model.BackupPending{Artifact: testGeneration(), DiskIdentity: witness.DiskIdentity}
	p.Artifact.Dependencies = witness.Dependencies
	if err := checkWitness(p, witness); err != nil {
		t.Fatal(err)
	}
	witness.RootBytes++
	if err := checkWitness(p, witness); err == nil {
		t.Fatal("capacity change accepted")
	}
	link := filepath.Join(c.Root, "kernel")
	if err := os.Symlink(c.Disk, link); err != nil {
		t.Fatal(err)
	}
	c.Dependencies = []string{link}
	if _, err := c.Inspect(context.Background(), lock, false); err == nil {
		t.Fatal("linked dependency accepted")
	}
}
func TestReflinkUnsupportedNeverFallsBackAndSupportedRestoreKeepsSource(t *testing.T) {
	requireIntegration(t)
	c := testBackupContext(t.TempDir())
	probeErr := ProbeReflink(c.Root)
	if probeErr != nil && !strings.HasPrefix(probeErr.Error(), "backup unsupported:") {
		t.Fatal(probeErr)
	}
	if entries, err := os.ReadDir(c.Root); err != nil || len(entries) != 0 {
		t.Fatal("qualification left scratch entries", entries, err)
	}
	writeTestFile(t, c.Disk, []byte(strings.Repeat("A", 4096)))
	lock, err := c.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	witness, err := c.Inspect(context.Background(), lock, false)
	if err != nil {
		t.Fatal(err)
	}
	artifact := testGeneration()
	artifact.Dependencies = witness.Dependencies
	pending := model.BackupPending{Request: model.BackupRequest{OperationID: uuid.NewString(), ExpectedRevision: uuid.NewString(), Action: model.BackupCreate}, Artifact: artifact, DiskIdentity: witness.DiskIdentity}
	point, err := c.Create(context.Background(), lock, pending)
	if err != nil {
		if os.Getenv("TT_TEST_REQUIRE_REFLINK") == "1" {
			t.Fatal("explicitly requested native reflink support is unavailable", err)
		}
		if !strings.HasPrefix(err.Error(), "backup unsupported:") {
			t.Fatal(err)
		}
		dir, e := c.artifactPath(artifact.ID)
		if e != nil {
			t.Fatal(e)
		}
		if _, e := os.Stat(filepath.Join(dir, "ready.json")); !os.IsNotExist(e) {
			t.Fatal("published failed reflink", e)
		}
		info, e := os.Stat(filepath.Join(dir, "disk"))
		if e != nil || info.Size() != 0 {
			t.Fatal("fell back to copying", e)
		}
		source, _ := os.ReadFile(c.Disk)
		if string(source) != strings.Repeat("A", 4096) {
			t.Fatal("changed source")
		}
		if e := c.Remove(context.Background(), lock, artifact); e != nil {
			t.Fatal(e)
		}
		t.Log("strict reflink unavailable; rejection branch verified, supported restore remains unverified on this filesystem")
		return
	}
	if probeErr != nil {
		t.Fatal("anonymous qualification rejected a supported filesystem", probeErr)
	}
	writeTestFile(t, c.Disk, []byte(strings.Repeat("B", 4096)))
	witness, err = c.Inspect(context.Background(), lock, false)
	if err != nil {
		t.Fatal(err)
	}
	restore := pending
	restore.Request.Action = model.BackupRestore
	restore.Request.Generation = &point.ID
	restore.Artifact.ID = uuid.NewString()
	restore.Artifact.RestoreStaging = true
	restore.DiskIdentity = witness.DiskIdentity
	stage, err := c.Restore(context.Background(), lock, restore, point)
	if err != nil || stage == nil {
		t.Fatal(stage, err)
	}
	data, _ := os.ReadFile(c.Disk)
	if string(data) != strings.Repeat("A", 4096) {
		t.Fatal("restore differs")
	}
	if _, err := c.Restore(context.Background(), lock, restore, point); err != nil {
		t.Fatal("lost-publication retry", err)
	}
	if err := c.Remove(context.Background(), lock, *stage); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, c.Disk, []byte(strings.Repeat("C", 4096)))
	dir, _ := c.artifactPath(point.ID)
	data, _ = os.ReadFile(filepath.Join(dir, "disk"))
	if string(data) != strings.Repeat("A", 4096) {
		t.Fatal("backup changed with active disk")
	}
	if err := c.Remove(context.Background(), lock, point); err != nil {
		t.Fatal(err)
	}
}
func TestUnsupportedReflinkLeavesSourceAndDestinationUnchanged(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("non-Linux rejection contract")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	target := filepath.Join(dir, "target")
	writeTestFile(t, source, []byte("retained disk"))
	writeTestFile(t, target, []byte("existing destination"))
	src, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := reflink(src, dst); err == nil || !strings.HasPrefix(err.Error(), "backup unsupported:") {
		t.Fatal(err)
	}
	if err := ProbeReflink(dir); err == nil {
		t.Fatal("probe accepted")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "existing destination" {
		t.Fatal("destination changed")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatal("probe artifacts retained")
	}
}
func TestTruncatedInventoryOutputCannotProveBackupAbsent(t *testing.T) {
	c := testBackupContext(t.TempDir())
	lock, err := c.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	fakeTool(t, "backup-inventory", `head -c 4194304 /dev/zero`)
	_, err = c.command(context.Background(), lock, "backup-inventory")
	if err == nil || !strings.Contains(err.Error(), "inventory is unconfirmed") {
		t.Fatal(err)
	}
	c.Deadline = time.Now().Add(-time.Second)
	if _, err := c.command(context.Background(), lock, "this-command-must-not-run"); err == nil || !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatal(err)
	}
}

func TestStorageChildRetainsExclusionWhenAgentProcessDies(t *testing.T) {
	const childEnv = "TT_STORAGE_LOCK_CHILD"
	if root := os.Getenv(childEnv); root != "" {
		c := testBackupContext(root)
		lock, err := c.Lock()
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		_, err = c.command(context.Background(), lock, "sh", "-c", `printf ready > "$1"; sleep 1`, "backup-child", filepath.Join(root, "ready"))
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	root := t.TempDir()
	c := testBackupContext(root)
	process := exec.Command(os.Args[0], "-test.run=^TestStorageChildRetainsExclusionWhenAgentProcessDies$")
	process.Env = append(os.Environ(), childEnv+"="+root, "TMPDIR="+root, "GORACE=atexit_sleep_ms=0")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = process.Process.Kill(); _ = process.Wait() }()
	until := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "ready")); err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("storage child not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := process.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = process.Wait()
	if lock, err := c.Lock(); err == nil {
		lock.Close()
		t.Fatal("orphan storage child lost its lock")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "tt-stdout-") || strings.HasPrefix(entry.Name(), "tt-stderr-") {
			t.Fatal("capture file survived agent crash", entry.Name())
		}
	}
	for time.Now().Before(until) {
		if lock, err := c.Lock(); err == nil {
			lock.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("storage child did not release lock")
}
