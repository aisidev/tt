package ctl

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/backup"
	"github.com/aisidev/tt/internal/capability"
	"github.com/aisidev/tt/model"
)

func backupError(message string) *APIError { return apiError(backup.ErrorStatus(message), message) }
func (s *Server) backupTarget(ctx context.Context, id string) (*model.Vm, *model.Host, *APIError) {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	vm, err := s.db.GetVM(ctx, id)
	if err != nil {
		return nil, nil, internal(err)
	}
	if vm == nil {
		return nil, nil, backupError("not found: VM")
	}
	host, err := s.db.GetHost(ctx, vm.HostID)
	if err != nil {
		return nil, nil, internal(err)
	}
	if host == nil {
		return nil, nil, internal(fmt.Errorf("VM host missing"))
	}
	if host.State != model.HostOnline {
		return nil, nil, apiError(503, "VM host offline")
	}
	if !slices.Contains(host.Capabilities, capability.LegacyBackup) {
		return nil, nil, backupError("backup unsupported: agent lacks disk_backup_v1; upgrade agent and controller")
	}
	return vm, host, nil
}
func decodeBackup(response *http.Response) (api.BackupView, *APIError) {
	status := response.StatusCode
	result, err := decodeResponse[api.BackupView](response)
	if err != nil {
		return api.BackupView{}, apiError(502, "backup outcome unknown: invalid agent response: "+err.Error())
	}
	if !successful(status) || !result.OK {
		code := 502
		if status >= 400 && status < 500 {
			code = status
		}
		message := "backup outcome unknown: agent failure"
		if result.Error != nil {
			message = *result.Error
		}
		return api.BackupView{}, apiError(code, message)
	}
	if result.Data == nil {
		return api.BackupView{}, apiError(502, "backup outcome unknown: missing result")
	}
	return *result.Data, nil
}
func (s *Server) retainBackup(ctx context.Context, expected model.Vm, view api.BackupView) (api.BackupView, *APIError) {
	if view.Vm.ID != expected.ID || view.Vm.HostID != expected.HostID || view.Vm.EnvID != expected.EnvID {
		return api.BackupView{}, apiError(502, "backup outcome unknown: agent VM identity mismatch")
	}
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	known, err := s.db.GetVM(ctx, expected.ID)
	if err != nil {
		return api.BackupView{}, internal(err)
	}
	if known == nil {
		return api.BackupView{}, backupError("not found: VM removed during backup")
	}
	view.Vm = mergeVMSnapshot(*known, view.Vm)
	if err := s.db.PutVMObservation(ctx, &view.Vm, known.ReservedDisk()); err != nil {
		return api.BackupView{}, internal(err)
	}
	s.revision.Add(1)
	return view, nil
}
func (s *Server) inspectBackup(ctx context.Context, vm model.Vm, host model.Host) (api.BackupView, *APIError) {
	client, err := s.clientFactory(30 * time.Second)
	if err != nil {
		return api.BackupView{}, internal(err)
	}
	response, err := client.request(ctx, "GET", host.Addr, "/api/vms/"+vm.ID+"/backup", nil, nil)
	if err != nil {
		return api.BackupView{}, apiError(502, "cannot inspect backup: "+err.Error())
	}
	view, apiErr := decodeBackup(response)
	if apiErr != nil {
		return api.BackupView{}, apiErr
	}
	return s.retainBackup(ctx, vm, view)
}
func (s *Server) backupMutation(ctx context.Context, id string, headers http.Header, action model.BackupAction, generation *string) (api.BackupView, *APIError) {
	request, err := backup.FromHeaders(api.HeaderText(headers, "Idempotency-Key"), api.HeaderText(headers, "If-Match"), action, generation)
	if err != nil {
		return api.BackupView{}, backupError(err.Error())
	}
	vm, host, apiErr := s.backupTarget(ctx, id)
	if apiErr != nil {
		return api.BackupView{}, apiErr
	}
	operation, apiErr := s.beginOperation(vm.EnvID)
	if apiErr != nil {
		return api.BackupView{}, apiErr
	}
	defer operation.Close()
	view, apiErr := s.inspectBackup(ctx, *vm, *host)
	if apiErr != nil {
		return api.BackupView{}, apiErr
	}
	completed, err := backup.Check(view.Vm.Backup, request)
	if err != nil {
		return api.BackupView{}, backupError(err.Error())
	}
	if completed {
		if view.Vm.Backup.LastResult != nil && view.Vm.Backup.LastResult.Error != nil {
			return api.BackupView{}, backupError(*view.Vm.Backup.LastResult.Error)
		}
		return view, nil
	}
	if forwarded := view.Vm.Backup.Forwarding; forwarded != nil && !backup.Equal(*forwarded, request) {
		return api.BackupView{}, backupError("conflict: another backup outcome is unresolved")
	}
	previous := view.Vm
	fresh := previous.Backup.Forwarding == nil && previous.Backup.Pending == nil
	if previous.State != model.VmStopped || previous.PendingResources != nil {
		return api.BackupView{}, backupError("conflict: stop VM and resolve resource updates before backup")
	}
	if action == model.BackupCreate && fresh {
		if !view.Enabled {
			return api.BackupView{}, backupError("backup admission disabled")
		}
		if err := capability.RequireHost(*host, vm.Engine, []capability.Feature{capability.Backup}); err != nil {
			return api.BackupView{}, backupError("backup unsupported: " + err.Error())
		}
		if !view.Supported {
			message := "backup unsupported: runtime"
			if view.UnsupportedReason != nil {
				message = *view.UnsupportedReason
			}
			return api.BackupView{}, backupError(message)
		}
	}
	if apiErr := s.recordBackupForwarding(ctx, id, *vm, *host, request, fresh); apiErr != nil {
		return api.BackupView{}, apiErr
	}
	method, suffix := "PUT", ""
	if action == model.BackupRestore {
		method = "POST"
		suffix = "/restore"
	} else if action == model.BackupDelete {
		method = "DELETE"
	}
	client, err := s.clientFactory(360 * time.Second)
	if err != nil {
		return api.BackupView{}, internal(err)
	}
	outboundHeaders := http.Header{}
	outboundHeaders.Set("Idempotency-Key", request.OperationID)
	outboundHeaders.Set("If-Match", "\""+request.ExpectedRevision+"\"")
	var body any
	if request.Generation != nil {
		body = api.RestoreBody{Generation: *request.Generation}
	}
	var settled *uint64
	response, err := client.request(ctx, method, host.Addr, "/api/vms/"+id+"/backup"+suffix, body, outboundHeaders)
	var result api.BackupView
	if err != nil {
		apiErr = apiError(502, fmt.Sprintf("backup outcome unknown; inspect and retry operation %s: %v", request.OperationID, err))
	} else {
		token := api.HeaderText(response.Header, "x-tt-backup-settled")
		sequence := api.HeaderText(response.Header, "x-tt-backup-settled-sequence")
		if token != nil && *token == request.OperationID && sequence != nil {
			if value, err := strconv.ParseUint(strings.TrimPrefix(*sequence, "+"), 10, 64); err == nil {
				settled = &value
			}
		}
		result, apiErr = decodeBackup(response)
	}
	if apiErr == nil {
		return s.retainBackup(ctx, *vm, result)
	}
	if settled != nil {
		_, err := withDB(s, func(db *DB) (model.Unit, error) {
			saved, err := db.GetVM(ctx, id)
			if err != nil {
				return model.Unit{}, err
			}
			if saved != nil && saved.Backup.Forwarding != nil && backup.Equal(*saved.Backup.Forwarding, request) {
				saved.Backup.ForwardingSettledSequence = settled
				if err := db.PutVM(ctx, saved); err != nil {
					return model.Unit{}, err
				}
				s.revision.Add(1)
			}
			return model.Unit{}, nil
		})
		if err != nil {
			return api.BackupView{}, internal(err)
		}
	}
	_, _ = s.inspectBackup(ctx, *vm, *host)
	return api.BackupView{}, apiErr
}
func (s *Server) recordBackupForwarding(ctx context.Context, id string, vm model.Vm, host model.Host, request model.BackupRequest, fresh bool) *APIError {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	env, err := s.db.GetEnv(ctx, vm.EnvID)
	if err != nil {
		return internal(err)
	}
	if env == nil {
		return backupError("not found: environment")
	}
	if env.State == model.EnvCreating || env.State == model.EnvDeleting {
		return backupError("conflict: environment lifecycle operation unfinished")
	}
	saved, err := s.db.GetVM(ctx, id)
	if err != nil {
		return internal(err)
	}
	if saved == nil {
		return backupError("not found: VM")
	}
	if fresh {
		reserve := uint32(0)
		if request.Action != model.BackupDelete {
			reserve = model.SatSub(saved.Disk, saved.Options.ConfigDiskMiB(saved.Engine))
		}
		current, err := s.db.GetHost(ctx, host.ID)
		if err != nil {
			return internal(err)
		}
		if current == nil {
			return internal(fmt.Errorf("host missing"))
		}
		if current.Resource.DiskFree() < reserve {
			return backupError("conflict: insufficient backup disk budget")
		}
		saved.Backup.ForwardingReservedMiB = reserve
		if current.Resource.DiskUsed > math.MaxUint32-reserve {
			return backupError("conflict: backup reservation overflow")
		}
		current.Resource.DiskUsed += reserve
		if err := s.db.PutHost(ctx, current); err != nil {
			return internal(err)
		}
	}
	saved.Backup.Forwarding = &request
	if err := s.db.PutVM(ctx, saved); err != nil {
		return internal(err)
	}
	s.revision.Add(1)
	return nil
}
