package ctl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
)

func TestImageAcceptancePublishesObservedCatalogBeforeScheduling(t *testing.T) {
	ctx := context.Background()
	s := newServer(testDB(t), nil)
	id := "sha256-" + strings.Repeat("d", 64) + "-g1"
	info := api.AgentInfo{Storage: model.File, Engines: model.List[model.Engine]{model.Qemu}, HostID: "host", Images: model.List[string]{id}, ImageSizes: model.Map[string, uint32]{id: 16}, Resource: model.Resource{DiskUsed: 96}}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { api.WriteJSON(w, 200, api.Success(info)) }))
	defer peer.Close()
	host := managedHost()
	host.Addr = strings.TrimPrefix(peer.URL, "http://")
	host.Resource.DiskUsed = 128
	if err := s.db.PutHost(ctx, &host); err != nil {
		t.Fatal(err)
	}
	client, err := newAgentClient(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.confirmImageCatalog(ctx, client, imageTarget{host.ID, host.Addr}, id, 16); err != nil {
		t.Fatal(err)
	}
	saved, err := s.db.GetHost(ctx, host.ID)
	if err != nil || len(saved.Images) != 1 || saved.Images[0] != id || saved.ImageSizes[id] != 16 || saved.Resource.DiskUsed != 128 {
		t.Fatal(saved, err)
	}
	info.HostID = "replacement"
	if err = s.confirmImageCatalog(ctx, client, imageTarget{host.ID, host.Addr}, id, 16); err == nil {
		t.Fatal("replacement observation changed known host catalog")
	}
}

func TestImagePlanPinsCompleteReceiverSetAndImmutableProfile(t *testing.T) {
	ctx := context.Background()
	s := newServer(testDB(t), nil)
	h := managedHost()
	h.StorageFeatures.ImagePull = true
	h.StorageFeatures.ImageCatalog = true
	if err := s.db.PutHost(ctx, &h); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("b", 64)
	id := "sha256-" + digest + "-g1"
	request := api.ArchiveImportRequest{Pull: model.ArchivePull{ID: id, SHA256: digest, Bytes: 100, ExpandedMiB: 96}, Spec: model.VolumeSpec{Engine: model.Qemu, SizeMiB: 16}}
	plan, err := s.planImage(ctx, id, request, false)
	if err != nil || len(plan.Targets) != 1 || plan.Targets[0].ID != h.ID {
		t.Fatal(plan, err)
	}
	// Host catalog changes cannot redirect a replay or shrink retirement's set.
	h.State = model.HostOffline
	if err = s.db.PutHost(ctx, &h); err != nil {
		t.Fatal(err)
	}
	extra := managedHost()
	extra.ID = "new-host"
	extra.StorageFeatures.ImagePull = true
	extra.StorageFeatures.ImageCatalog = true
	if err = s.db.PutHost(ctx, &extra); err != nil {
		t.Fatal(err)
	}
	again, err := s.planImage(ctx, id, request, false)
	if err != nil || len(again.Targets) != 1 || again.Targets[0] != plan.Targets[0] {
		t.Fatal(again, err)
	}
	// A fresh authorized distribution expands the set without forgetting the
	// offline predecessor. Its replay freezes that expanded set.
	nextCtx := command.WithContext(ctx, command.Envelope{Subject: id, Key: "explicit-redistribution", Token: 2, Generation: 1, Fingerprint: strings.Repeat("e", 64)})
	expanded, err := s.planImage(nextCtx, id, request, false)
	if err != nil || len(expanded.Targets) != 2 || expanded.Targets[0] != plan.Targets[0] {
		t.Fatal(expanded, err)
	}
	changed := request
	changed.Spec.Engine = model.Firecracker
	if _, err = s.planImage(ctx, id, changed, false); err == nil {
		t.Fatal("changed image profile accepted")
	}
	retired, err := s.planImage(ctx, id, api.ArchiveImportRequest{}, true)
	if err != nil || !retired.Deleting || len(retired.Targets) != 2 || retired.Targets[0] != plan.Targets[0] {
		t.Fatal(retired, err)
	}
	if _, err = s.planImage(ctx, id, request, false); err == nil {
		t.Fatal("retired catalog dispatched another pull")
	}
}
func TestImageProfileRequiresNegotiatedCatalog(t *testing.T) {
	ctx := context.Background()
	s := newServer(testDB(t), nil)
	h := managedHost()
	h.StorageFeatures.ImagePull = true
	if err := s.db.PutHost(ctx, &h); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("c", 64)
	id := "sha256-" + digest + "-g1"
	request := api.ArchiveImportRequest{Pull: model.ArchivePull{ID: id, SHA256: digest, Bytes: 100, ExpandedMiB: 96}, Spec: model.VolumeSpec{Engine: model.Qemu, SizeMiB: 16}}
	if _, err := s.planImage(ctx, id, request, false); err == nil {
		t.Fatal("unknown image profile feature accepted")
	}
}
