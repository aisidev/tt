package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aisidev/tt/internal/storage"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

func TestNativeZFSInventoryUsesLogicalCapacityAndRequiresRootChild(t *testing.T) {
	parent := os.Getenv("TT_NATIVE_INVENTORY_PARENT")
	if parent == "" {
		t.Skip("set an explicit task-owned ZFS parent on an authorized host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	zfs := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "zfs", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("owned ZFS fixture command failed: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if zfs("get", "-Hp", "-o", "value", "tt:validation-owner", parent) != "om-refactor" {
		t.Fatal("native inventory parent is not owned by this validation task")
	}
	dataset := parent + "/inventory-" + uuid.NewString()[:8]
	zfs("create", "-o", "quota=512M", "-o", "tt:validation-owner=om-refactor", dataset)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cleanup, "zfs", "get", "-Hp", "-o", "value", "tt:validation-owner", dataset).CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "om-refactor" {
			t.Error("fixture ownership unavailable; dataset retained", dataset)
			return
		}
		if out, err = exec.CommandContext(cleanup, "zfs", "destroy", "-r", dataset).CombinedOutput(); err != nil {
			t.Error("owned dataset cleanup failed", dataset, err, string(out))
		}
	})
	zfs("create", dataset+"/images")
	zfs("create", "-s", "-V", "32M", dataset+"/volume-known-g1")
	zfs("create", "-s", "-V", "64M", dataset+"/volume-orphan-g1")
	zfs("create", dataset+"/volume-fc-missing-g1")
	zfs("snapshot", dataset+"/volume-known-g1@point-known-g1")
	s, _, _ := serverFixture(t)
	s.runtime.store = storage.ZvolStore{}
	s.runtime.storage = model.Zvol
	s.runtime.runtimeDir = dataset
	s.runtime.imageDir = dataset + "/images"
	s.imageDir = s.runtime.imageDir
	s.runtime.runDir = t.TempDir()
	known := model.Volume{ID: "known-g1", HostID: s.runtime.hostID, Owner: "test", Generation: 1, SizeMiB: 16, Format: "raw", State: "ready", Phase: "ready"}
	missing := model.Volume{ID: "fc-missing-g1", HostID: s.runtime.hostID, Owner: "test", Generation: 1, SizeMiB: 16, Image: "fixture", Engine: model.Firecracker, Format: "raw", State: "ready", Phase: "ready"}
	point := model.VolumeSnapshot{ID: "point-known-g1", VolumeID: known.ID, HostID: s.runtime.hostID, Generation: 1, State: "ready", Phase: "ready", Source: known}
	for _, volume := range []model.Volume{known, missing} {
		if err := saveDisk(ctx, s.runtime.db.conn, "volumes", volume.ID, volume); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveDisk(ctx, s.runtime.db.conn, "volume_snapshots", point.ID, point); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.physicalInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.QuarantinePinned || snapshot.QuarantineMiB != 96 {
		t.Fatal("logical volume/snapshot capacity was lost or parent usage double-counted", snapshot.QuarantineMiB, snapshot.QuarantinePinned)
	}
	found := false
	for _, item := range snapshot.Items {
		if item.ID == missing.ID && item.Kind == "volume" {
			found = true
			if item.Problem != "payload_missing" {
				t.Fatal("parent filesystem hid missing rootfs child", item)
			}
		}
	}
	if !found {
		t.Fatal("missing root payload disappeared from inventory")
	}
}
