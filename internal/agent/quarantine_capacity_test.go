//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/storage"
	"github.com/aisidev/tt/model"
)

func TestPhysicalQuarantineAndLostGrowReceiptConstrainAdmissionAndInfo(t *testing.T) {
	s, _, _ := serverFixture(t)
	r := s.runtime
	r.store = storage.FileStore{}
	r.runDir = filepath.Join(t.TempDir(), "run")
	for _, path := range []string{r.runtimeDir, r.imageDir, r.runDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	r.resource.DiskTotal = 1200
	s.info.Resource.DiskTotal = 1200
	ctx := context.Background()
	if _, err := r.CreateVolume(ctx, model.VolumeSpec{ID: "owned-g1", Generation: 1, SizeMiB: 64}); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(r.volumePath("owned-g1"), 128<<20); err != nil {
		t.Fatal(err)
	}
	orphan := r.volumePath("orphan-g1")
	file, err := os.Create(orphan)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	file.Close()
	snapshot, err := s.physicalInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.QuarantinePinned || snapshot.QuarantineMiB != 1088 {
		t.Fatal("lost rows/grow receipt released physical capacity", snapshot.QuarantineMiB, snapshot.QuarantinePinned)
	}
	r.installQuarantine(model.ActivationReceipt{QuarantineMiB: snapshot.QuarantineMiB})
	if err = r.recount(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = r.CreateVolume(ctx, model.VolumeSpec{ID: "new-g1", Generation: 1, SizeMiB: 100})
	var boundary *api.BoundaryError
	if !errors.As(err, &boundary) || boundary.Code != "capacity_exceeded" {
		t.Fatal("new allocation ignored retained bytes", err)
	}
	response := httptest.NewRecorder()
	s.getInfo(response, httptest.NewRequest("GET", "/api/info", nil))
	var info api.Resp[api.AgentInfo]
	if json.Unmarshal(response.Body.Bytes(), &info) != nil || info.Data == nil || info.Data.Resource.DiskUsed != 1152 {
		t.Fatal("advertised capacity disagreed with admission", response.Body.String())
	}
	if err = os.Remove(orphan); err != nil {
		t.Fatal(err)
	}
	if err = r.recount(ctx); err != nil {
		t.Fatal(err)
	}
	if r.resource.DiskUsed != 1152 {
		t.Fatal("a disappearing file silently released quarantine authority", r.resource.DiskUsed)
	}
}
func TestUnknownDiskLayoutPinsCapacity(t *testing.T) {
	s, _, _ := serverFixture(t)
	r := s.runtime
	r.store = storage.FileStore{}
	r.runDir = filepath.Join(t.TempDir(), "run")
	for _, path := range []string{r.runtimeDir, r.imageDir, r.runDir, filepath.Join(r.runtimeDir, "volume-unknown-g1")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(r.runtimeDir, "volume-unknown-g1", "unexpected"), []byte("held"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.physicalInventory(context.Background())
	if err != nil || !snapshot.QuarantinePinned {
		t.Fatal("unbounded payload became free capacity", err)
	}
	r.installQuarantine(model.ActivationReceipt{QuarantinePinned: true})
	if err = r.recount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.resource.CanFit(0, 0, 1) {
		t.Fatal("unknown physical envelope permitted allocation")
	}
}
