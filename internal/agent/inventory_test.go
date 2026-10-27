package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/storage"
	"github.com/aisidev/tt/model"
)

func TestPhysicalInventoryFindsUnownedMissingPayloadAndDoesNotTraverseLinks(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("physical process inventory is supported only on Linux")
	}
	s, _, _ := serverFixture(t)
	s.runtime.store = storage.FileStore{}
	for _, path := range []string{s.runtime.runtimeDir, s.imageDir, s.runtime.runDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	for _, id := range []string{"owned-g1", "missing-g1"} {
		if _, err := s.runtime.CreateVolume(ctx, model.VolumeSpec{ID: id, Generation: 1, SizeMiB: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(s.runtime.volumePath("missing-g1")); err != nil {
		t.Fatal(err)
	}
	orphan := s.runtime.volumePath("orphan-g1")
	if err := os.WriteFile(orphan, []byte("unowned-bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(external, []byte("secret-must-not-leak"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, s.runtime.volumePath("link-g1")); err != nil {
		t.Fatal(err)
	}
	root := model.Volume{ID: "broken-root-g1", HostID: s.runtime.hostID, Owner: "owner", Generation: 1, Engine: model.Firecracker, Image: "fixture", SizeMiB: 1, State: "ready", Phase: "ready", Format: "raw", CreatedAt: 1}
	if err := saveDisk(ctx, s.runtime.db.conn, "volumes", root.ID, root); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.runtime.volumePath(root.ID), 0700); err != nil {
		t.Fatal(err)
	}
	result, err := s.physicalInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]model.InventoryItem{}
	for _, item := range result.Items {
		found[item.Kind+"/"+item.ID] = item
	}
	if item := found["volume/orphan-g1"]; item.Metadata || !item.Physical {
		t.Fatal("orphan was adopted or missed", item)
	}
	if item := found["volume/missing-g1"]; !item.Metadata || item.Physical {
		t.Fatal("missing owned bytes were fabricated", item)
	}
	if item := found["volume/broken-root-g1"]; item.Problem != "payload_missing" {
		t.Fatal("existing directory hid missing payload", item)
	}
	if item := found["volume/link-g1"]; item.Problem != "unowned_link" {
		t.Fatal("unowned link was followed", item)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "secret-must-not-leak") {
		t.Fatal("inventory disclosed link target contents")
	}
	bytes, err := os.ReadFile(orphan)
	if err != nil || string(bytes) != "unowned-bytes" {
		t.Fatal("inventory changed unowned bytes", err)
	}
	if result.Frozen || !result.Complete {
		t.Fatal("unfenced inventory was promoted to recovery proof", result.Frozen, result.Complete)
	}
}
func TestPhysicalInventoryRejectsCatalogRowsWithoutAState(t *testing.T) {
	s, _, _ := serverFixture(t)
	if _, err := s.runtime.db.conn.ExecContext(context.Background(), "INSERT INTO volumes(id,data) VALUES(?1,?2)", "lost-state-g1", `{"id":"lost-state-g1"}`); err != nil {
		t.Fatal(err)
	}
	_, err := s.physicalInventory(context.Background())
	var typed *api.BoundaryError
	if !errors.As(err, &typed) || typed.Code != "malformed" {
		t.Fatal("invalid JSON shape was silently omitted from complete inventory", err)
	}
}

func TestPhysicalInventoryUnsupportedPlatformsFailClosed(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("physical process inventory is supported on Linux")
	}
	s, _, _ := serverFixture(t)
	s.runtime.store = storage.FileStore{}
	for _, path := range []string{s.runtime.runtimeDir, s.imageDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := s.physicalInventory(context.Background())
	var typed *api.BoundaryError
	if !errors.As(err, &typed) || typed.Code != "inventory_unavailable" || snapshot.Complete {
		t.Fatal("unsupported process inspection became complete recovery evidence", snapshot, err)
	}
}

func TestInventoryPagingUsesOneCapturedIdentityAndBoundedPages(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("physical process inventory is supported only on Linux")
	}
	s, _, _ := serverFixture(t)
	s.runtime.store = storage.FileStore{}
	for _, path := range []string{s.runtime.runtimeDir, s.imageDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"a-g1", "b-g1"} {
		if _, err := s.runtime.CreateVolume(context.Background(), model.VolumeSpec{ID: id, Generation: 1, SizeMiB: 1}); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest("GET", "/api/control/inventory?size=1", nil)
	first := httptest.NewRecorder()
	s.physicalInventoryHTTP(first, request)
	var page api.Resp[model.PhysicalInventory]
	if json.Unmarshal(first.Body.Bytes(), &page) != nil || first.Code != 200 || page.Data == nil || len(page.Data.Items) != 1 || page.Data.NextToken == "" {
		t.Fatal(first.Code, first.Body.String())
	}
	snapshot := page.Data.ID
	second := httptest.NewRecorder()
	s.physicalInventoryHTTP(second, httptest.NewRequest("GET", "/api/control/inventory?size=1&snapshot="+snapshot+"&after="+page.Data.NextToken, nil))
	var next api.Resp[model.PhysicalInventory]
	if json.Unmarshal(second.Body.Bytes(), &next) != nil || next.Data == nil || next.Data.ID != snapshot || next.Data.Digest != page.Data.Digest || len(next.Data.Items) != 1 {
		t.Fatal(second.Code, second.Body.String())
	}
	invalid := httptest.NewRecorder()
	s.physicalInventoryHTTP(invalid, httptest.NewRequest("GET", "/api/control/inventory?after=1", nil))
	if invalid.Code != 400 {
		t.Fatal("identity-less cursor was accepted", invalid.Code)
	}
}
