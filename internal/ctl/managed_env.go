package ctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
)

func managedRequest(request api.CreateEnvReq) bool {
	for _, spec := range request.VMs {
		if spec.DurableRoot || spec.RootVolumeID != "" || len(spec.DataVolumeIDs) > 0 || spec.InitialStopped {
			return true
		}
	}
	return false
}
func creationHash(request api.CreateEnvReq) string {
	raw, _ := json.Marshal(request)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func managedRootID(environment string, index int, generation uint64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", environment, index)))
	return fmt.Sprintf("root-%x-g%d", sum[:16], generation)
}

// prepareDiskPlacement runs under dbMu. Existing bytes pin every attached disk
// to one host; neither a stale hint nor a retry can choose a second locality.
func (s *Server) prepareDiskPlacement(ctx context.Context, spec api.VmSpec) (api.VmSpec, error) {
	ids := append([]string{}, spec.DataVolumeIDs...)
	if spec.RootVolumeID != "" {
		ids = append(ids, spec.RootVolumeID)
	}
	if len(spec.DataVolumeIDs) > 8 {
		return spec, &api.BoundaryError{Class: "terminal", Code: "size_exceeded", Message: "at most eight data volumes are supported"}
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return spec, &api.BoundaryError{Class: "terminal", Code: "invalid_volume", Message: "duplicate disk attachment"}
		}
		seen[id] = true
		volume, err := s.db.GetVolume(ctx, id)
		if err != nil {
			return spec, err
		}
		if volume == nil {
			return spec, &api.BoundaryError{Class: "terminal", Code: "not_found", Message: "requested disk is not in the catalog"}
		}
		if volume.State != "ready" || volume.AttachedTo != "" || volume.PendingSnapshot != "" {
			return spec, &api.BoundaryError{Class: "state", Code: "volume_busy", Message: "requested disk is not available for binding"}
		}
		if spec.HostID != "" && spec.HostID != volume.HostID {
			return spec, &api.BoundaryError{Class: "state", Code: "wrong_locality", Message: "all disks must share one pinned host"}
		}
		spec.HostID = volume.HostID
		if id == spec.RootVolumeID {
			if volume.Engine != spec.Engine || volume.Image != spec.Image {
				return spec, &api.BoundaryError{Class: "terminal", Code: "invalid_volume", Message: "root image and engine must match"}
			}
			if spec.Disk != nil && *spec.Disk != volume.SizeMiB {
				return spec, &api.BoundaryError{Class: "terminal", Code: "invalid_volume", Message: "root capacity must match the existing volume"}
			}
			size := volume.SizeMiB
			spec.Disk = &size
		} else if volume.Engine != "" || volume.Format != "raw" {
			return spec, &api.BoundaryError{Class: "terminal", Code: "invalid_volume_role", Message: "data attachment requires a raw data disk"}
		}
	}
	return spec, nil
}
func (s *Server) observeRequestedDisks(ctx context.Context, request api.CreateEnvReq) *APIError {
	for _, spec := range request.VMs {
		ids := append([]string{}, spec.DataVolumeIDs...)
		if spec.RootVolumeID != "" {
			ids = append(ids, spec.RootVolumeID)
		}
		for _, id := range ids {
			if _, err := s.getVolume(ctx, id); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *Server) resumeManagedPlan(ctx context.Context, env *model.Env, request api.CreateEnvReq) ([]creationRequest, *APIError) {
	if env.CreationHash == "" || env.CreationHash != creationHash(request) || env.State == model.EnvDeleting {
		return nil, diskAPIError("terminal", "fingerprint_conflict", "environment creation intent does not match")
	}
	if len(env.VmIds) != len(request.VMs) {
		return nil, diskAPIError("unknown", "plan_incomplete", "environment plan cannot be established; retained")
	}
	requests := []creationRequest{}
	for index, id := range env.VmIds {
		vm, err := s.db.GetVM(ctx, id)
		if err != nil {
			return nil, internal(err)
		}
		if vm == nil {
			return nil, diskAPIError("unknown", "plan_incomplete", "planned VM record is missing; retained")
		}
		host, err := s.db.GetHost(ctx, vm.HostID)
		if err != nil {
			return nil, internal(err)
		}
		if host == nil {
			return nil, diskAPIError("transient", "host_unavailable", "pinned host is unavailable; retained")
		}
		spec := request.VMs[index]
		owner := request.Owner
		if owner == "" {
			owner = request.ID
		}
		req := api.CreateVmReq{VmID: vm.ID, EnvID: vm.EnvID, HostID: vm.HostID, VolumeOwner: owner, Image: vm.Image, Engine: vm.Engine, CPU: vm.CPU, Mem: vm.Mem, Disk: vm.Options.RequestedDisk, Ports: spec.Ports, DenyOutgoing: spec.DenyOutgoing, IsolatedNetwork: spec.IsolatedNetwork, SSH: spec.SSH, SSHKeys: spec.SSHKeys, GuestConfig: spec.GuestConfig, DurableRoot: spec.DurableRoot, RootVolumeID: vm.Options.RootVolumeID, DataVolumeIDs: spec.DataVolumeIDs, RootRetain: spec.RootRetain, InitialStopped: spec.InitialStopped}
		requests = append(requests, creationRequest{address: host.Addr, request: req})
	}
	return requests, nil
}
func commandGeneration(ctx context.Context) uint64 {
	envelope, ok := command.FromContext(ctx)
	if !ok {
		return 0
	}
	return envelope.Generation
}
