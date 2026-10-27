//go:build linux || freebsd

package storage

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

// The caller supplies a dedicated, quota-limited parent dataset. This test only
// creates and removes its own UUID-named child, never an existing disk or parent.
func TestNativeZvolBackupRestoreAndOwnership(t *testing.T) {
	parent := os.Getenv("TT_TEST_ZVOL_PARENT")
	if parent == "" {
		t.Skip("no dedicated ZFS parent supplied")
	}
	requireIntegration(t, "zfs")
	ctx := context.Background()
	dataset := parent + "/disk-" + uuid.NewString()
	if _, err := zfsCommand(ctx, "create", "-V", "16M", "-o", "volmode=dev", dataset); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := zfsCommand(ctx, "destroy", "-r", dataset); err != nil {
			t.Error("test volume cleanup", err)
		}
	}()
	store := ZvolStore{}
	device, err := store.device(ctx, dataset)
	if err != nil {
		t.Fatal(err)
	}
	writeMarker := func(text string) {
		t.Helper()
		file, err := os.OpenFile(device, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = file.WriteAt([]byte(text), 0)
		closeErr := file.Close()
		if err != nil {
			t.Fatal(err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	readMarker := func() string {
		t.Helper()
		file, err := os.Open(device)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		data := make([]byte, 8)
		if _, err := file.ReadAt(data, 0); err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	writeMarker("before-A")
	root := t.TempDir()
	c := testBackupContext(root)
	c.Engine = model.Bhyve
	c.Backend = model.BackupZvol
	c.Disk = dataset
	c.Deadline = time.Now().Add(120 * time.Second)
	lock, err := c.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	witness, err := c.Inspect(ctx, lock, true)
	if err != nil {
		t.Fatal(err)
	}
	g := testGeneration()
	g.Backend = model.BackupZvol
	g.RootBytes = witness.RootBytes
	g.Dependencies = witness.Dependencies
	pending := model.BackupPending{Artifact: g, DiskIdentity: witness.DiskIdentity, Request: model.BackupRequest{Action: model.BackupCreate, OperationID: uuid.NewString(), ExpectedRevision: uuid.NewString()}}
	point, err := c.Create(ctx, lock, pending)
	if err != nil {
		t.Fatal(err)
	}
	writeMarker("after--B")
	pending.Request.Action = model.BackupRestore
	pending.Request.Generation = &point.ID
	if _, err := c.Restore(ctx, lock, pending, point); err != nil {
		t.Fatal(err)
	}
	if got := readMarker(); got != "before-A" {
		t.Fatalf("restore marker=%q", got)
	}
	foreign := dataset + "@foreign-" + uuid.NewString()
	if _, err := zfsCommand(ctx, "snapshot", foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PrepareRestore(ctx, lock, point, nil); err == nil || !strings.Contains(err.Error(), "foreign snapshot") {
		t.Fatal(err)
	}
	if !zfsOK(ctx, "list", "-t", "snapshot", foreign) {
		t.Fatal("foreign snapshot removed")
	}
	if _, err := zfsCommand(ctx, "destroy", foreign); err != nil {
		t.Fatal(err)
	}
	snap, _ := c.snapshot(point)
	if _, err := zfsCommand(ctx, "set", "ttstack:backup=foreign-owner", snap); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(ctx, lock, point); err == nil || !strings.Contains(err.Error(), "ownership changed") {
		t.Fatal(err)
	}
	if _, err := zfsCommand(ctx, "set", "ttstack:backup="+c.VMID+":"+point.ID, snap); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(ctx, lock, point); err != nil {
		t.Fatal(err)
	}
	// Resize uses only the owned test volume and preserves its content.
	if err := store.ResizeDisk(ctx, dataset, 32); err != nil {
		t.Fatal(err)
	}
	if size, err := store.BhyveSize(ctx, dataset); err != nil || size != 32*1024*1024 {
		t.Fatal(size, err)
	}
	if got := readMarker(); got != "before-A" {
		t.Fatal("resize changed marker")
	}
	// Confirm the command runner itself remains bounded for native operations.
	if out, err := runcmd.Run(ctx, "zfs", "get", "-Hp", "-o", "value", "volsize", dataset); err != nil || !out.Success() {
		t.Fatal(err)
	}
}
