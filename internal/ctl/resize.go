package ctl

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/aisidev/tt/internal/capability"
	"github.com/aisidev/tt/model"
)

func (s *Server) resizeVM(ctx context.Context, id string, target model.VmResources) (*model.Vm, *APIError) {
	if target.CPU == 0 || target.Mem == 0 || target.Disk == 0 {
		return nil, apiError(400, "cpu, memory and root disk must be > 0")
	}
	original, err := withDB(s, func(db *DB) (*model.Vm, error) { return db.GetVM(ctx, id) })
	if err != nil {
		return nil, internal(err)
	}
	if original == nil {
		return nil, apiError(404, "VM not found")
	}
	operation, apiErr := s.beginOperation(original.EnvID)
	if apiErr != nil {
		return nil, apiErr
	}
	defer operation.Close()
	detail, apiErr := s.environmentDetail(ctx, original.EnvID)
	if apiErr != nil {
		return nil, apiErr
	}
	if detail.Env.State == model.EnvCreating || detail.Env.State == model.EnvDeleting {
		return nil, apiError(409, "environment has an operation in progress")
	}
	client, err := s.clientFactory(360 * time.Second)
	if err != nil {
		return nil, internal(err)
	}
	reader, err := s.clientFactory(5 * time.Second)
	if err != nil {
		return nil, internal(err)
	}
	s.refreshHosts(ctx, reader, &original.EnvID)
	vm, err := withDB(s, func(db *DB) (*model.Vm, error) { return db.GetVM(ctx, id) })
	if err != nil {
		return nil, internal(err)
	}
	if vm == nil {
		return nil, internal(fmt.Errorf("VM disappeared"))
	}
	host, err := withDB(s, func(db *DB) (*model.Host, error) { return db.GetHost(ctx, vm.HostID) })
	if err != nil {
		return nil, internal(err)
	}
	if host == nil {
		return nil, internal(fmt.Errorf("VM host missing"))
	}
	if host.State != model.HostOnline {
		return nil, apiError(503, "VM host is offline")
	}
	if err := capability.Resources.RequireDesign(vm.Engine, host.Storage); err != nil {
		return nil, apiError(400, err.Error())
	}
	if vm.PendingResources == nil {
		if err := capability.RequireHost(*host, vm.Engine, []capability.Feature{capability.Resources}); err != nil {
			return nil, apiError(400, err.Error())
		}
	}
	if vm.Backup.Busy() || (vm.Backup.HasArtifacts() && target.Disk != model.SatSub(vm.Disk, vm.Options.ConfigDiskMiB(vm.Engine))) {
		return nil, apiError(409, "backup operation or artifacts prevent this resource update; inspect/remove backups first")
	}
	if vm.State != model.VmStopped || (vm.PendingResources != nil && *vm.PendingResources != target) {
		return nil, apiError(409, "stop the VM first; an unfinished update must use its recorded target")
	}
	overhead := vm.Options.ConfigDiskMiB(vm.Engine)
	if target.Disk > math.MaxUint32-overhead {
		return nil, apiError(400, "disk overflow")
	}
	disk := target.Disk + overhead
	if target.Disk < vm.Options.RequestedDisk || disk < vm.Disk {
		return nil, apiError(400, "disk shrinking is not supported")
	}
	previous := *vm
	previousDisk := vm.ReservedDisk()
	vm.PendingResources = &target
	message := "resource update pending; retry the recorded target if interrupted"
	vm.Error = &message
	if _, err := withDB(s, func(db *DB) (model.Unit, error) {
		// Re-check capacity under the database lock so a concurrently planned
		// environment cannot slip through between the check and the intent.
		fresh, err := db.GetHost(ctx, host.ID)
		if err != nil {
			return model.Unit{}, err
		}
		if fresh == nil {
			return model.Unit{}, apiError(409, "the workspace host was removed")
		}
		if !fresh.Resource.CanFit(target.CPU, vm.Engine.MemoryReservation(target.Mem), model.SatSub(disk, previousDisk)) {
			return model.Unit{}, apiError(409, "insufficient resources on the workspace host")
		}
		if err := db.PutVMObservation(ctx, vm, previousDisk); err != nil {
			return model.Unit{}, err
		}
		s.revision.Add(1)
		return model.Unit{}, nil
	}); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return nil, apiErr
		}
		return nil, internal(err)
	}
	result, apiErr := s.sendResize(ctx, client, *host, *vm, previous, target, disk)
	reader, err = s.clientFactory(5 * time.Second)
	if err != nil {
		return nil, internal(err)
	}
	s.refreshHosts(ctx, reader, &original.EnvID)
	return result, apiErr
}
func (s *Server) sendResize(ctx context.Context, client *agentClient, host model.Host, vm, previous model.Vm, target model.VmResources, disk uint32) (*model.Vm, *APIError) {
	response, err := client.request(ctx, "POST", host.Addr, "/api/vms/"+vm.ID+"/resources", target, nil)
	if err != nil {
		return nil, apiError(502, "resource update outcome unknown; inspect VM before retrying: "+err.Error())
	}
	status := response.StatusCode
	reply, err := decodeResponse[model.Vm](response)
	if err != nil {
		return nil, apiError(502, "invalid agent response: "+err.Error())
	}
	if !successful(status) || !reply.OK {
		if previous.PendingResources == nil && (status == 400 || status == 409) {
			if _, err := withDB(s, func(db *DB) (model.Unit, error) {
				return model.Unit{}, db.PutVMObservation(ctx, &previous, vm.ReservedDisk())
			}); err != nil {
				return nil, internal(err)
			}
		}
		resultStatus := 502
		if status >= 400 && status < 500 {
			resultStatus = status
		}
		message := "resource update failed"
		if reply.Error != nil {
			message = *reply.Error
		}
		return nil, apiError(resultStatus, message)
	}
	confirmed := reply.Data
	if confirmed == nil {
		return nil, internal(fmt.Errorf("agent returned no VM"))
	}
	if confirmed.ID != vm.ID || confirmed.EnvID != vm.EnvID || confirmed.HostID != vm.HostID || confirmed.State != model.VmStopped || confirmed.PendingResources != nil || confirmed.CPU != target.CPU || confirmed.Mem != target.Mem || confirmed.Options.RequestedDisk != target.Disk || confirmed.Disk != disk {
		return nil, apiError(502, "agent returned inconsistent resource update")
	}
	if _, err := withDB(s, func(db *DB) (model.Unit, error) { return model.Unit{}, db.PutVM(ctx, confirmed) }); err != nil {
		return nil, internal(err)
	}
	return confirmed, nil
}
