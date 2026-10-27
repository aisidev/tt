package ctl

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/capability"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/internal/guestconfig"
	"github.com/aisidev/tt/internal/sshboot"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

type creationRequest struct {
	address string
	request api.CreateVmReq
}

func (s *Server) createEnvironment(ctx context.Context, request api.CreateEnvReq) (api.EnvDetail, *APIError) {
	if err := model.ValidateName(request.ID, "environment"); err != nil {
		return api.EnvDetail{}, apiError(400, err.Error())
	}
	if len(request.VMs) == 0 || len(request.VMs) > model.MaxVMs {
		return api.EnvDetail{}, apiError(400, fmt.Sprintf("an environment must contain 1..=%d VMs", model.MaxVMs))
	}
	request.VMs = append(model.List[api.VmSpec]{}, request.VMs...)
	for i := range request.VMs {
		spec := &request.VMs[i]
		spec.SSHKeys = append(append(model.List[string]{}, spec.SSHKeys...), request.SSHKeys...)
		slices.Sort(spec.SSHKeys)
		spec.SSHKeys = slices.Compact(spec.SSHKeys)
		settings, err := sshboot.ResolveOptions(spec.Engine, spec.SSH, spec.SSHKeys)
		if err != nil {
			return api.EnvDetail{}, apiError(400, err.Error())
		}
		spec.SSH = settings
		if _, present := spec.GuestConfig[sshboot.SeedFile]; present {
			return api.EnvDetail{}, apiError(400, "reserved guest configuration filename")
		}
		files := spec.GuestConfig
		if spec.Engine == model.Firecracker && spec.SSH != nil {
			// The agent merges the managed SSH seed into the same config disk
			// before re-validating, and the documented 32-file/64 KiB limit is
			// seed-inclusive; admission must judge the same payload.
			files = model.GuestConfig{}
			for name, value := range spec.GuestConfig {
				files[name] = value
			}
			files[sshboot.SeedFile] = ""
		}
		if err := guestconfig.Validate(spec.Engine, files, spec.IsolatedNetwork); err != nil {
			return api.EnvDetail{}, apiError(400, err.Error())
		}
		if err := model.ValidateImage(spec.Image, spec.Engine); err != nil {
			return api.EnvDetail{}, apiError(400, err.Error())
		}
		keys := append(append([]string{}, request.SSHKeys...), spec.SSHKeys...)
		if err := capability.ValidateVMOptions(spec.Engine, spec.Disk, spec.DenyOutgoing, keys, spec.Ports); err != nil {
			return api.EnvDetail{}, apiError(400, err.Error())
		}
		if (spec.CPU != nil && *spec.CPU == 0) || (spec.Mem != nil && *spec.Mem == 0) || (spec.Disk != nil && *spec.Disk == 0) {
			return api.EnvDetail{}, apiError(400, "cpu, memory and disk must be > 0 when specified")
		}
	}
	request.SSHKeys = nil
	if managedRequest(request) {
		if commandGeneration(ctx) == 0 {
			return api.EnvDetail{}, diskAPIError("terminal", "missing_identity", "managed environment requires fenced identity")
		}
		if failure := s.observeRequestedDisks(ctx, request); failure != nil {
			return api.EnvDetail{}, failure
		}
	}
	created := s.now()
	lifetime := model.DefaultLifetime
	if request.Lifetime != nil {
		lifetime = *request.Lifetime
	}
	expires := uint64(0)
	if lifetime > 0 {
		if lifetime > math.MaxUint64-created {
			return api.EnvDetail{}, apiError(400, "lifetime is too large")
		}
		expires = created + lifetime
	}
	operation, apiErr := s.beginOperation(request.ID)
	if apiErr != nil {
		return api.EnvDetail{}, apiErr
	}
	transferred := false
	defer func() {
		if !transferred {
			operation.Close()
		}
	}()
	reader, err := s.clientFactory(5 * time.Second)
	if err != nil {
		return api.EnvDetail{}, internal(err)
	}
	s.RefreshAllHosts(ctx, reader)
	requests, apiErr := s.planEnvironment(ctx, request, created, expires)
	if apiErr != nil {
		return api.EnvDetail{}, apiErr
	}
	detail, apiErr := s.environmentDetail(ctx, request.ID)
	if apiErr != nil {
		return api.EnvDetail{}, apiErr
	}
	if managedRequest(request) {
		if failure := s.finishCreation(ctx, request.ID, requests); failure != nil {
			return api.EnvDetail{}, failure
		}
		return s.environmentDetail(ctx, request.ID)
	}
	s.workers.Add(1)
	transferred = true
	go func() {
		defer s.workers.Done()
		defer operation.Close()
		if err := s.finishCreation(command.CopyContext(s.workCtx, ctx), request.ID, requests); err != nil && s.workCtx.Err() == nil {
			s.log("[ctl] creation: %s", err.Message)
		}
	}()
	return detail, nil
}
func (s *Server) planEnvironment(ctx context.Context, request api.CreateEnvReq, created, expires uint64) ([]creationRequest, *APIError) {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	existing, err := s.db.GetEnv(ctx, request.ID)
	if err != nil {
		return nil, internal(err)
	}
	if existing != nil {
		if managedRequest(request) {
			return s.resumeManagedPlan(ctx, existing, request)
		}
		return nil, apiError(409, fmt.Sprintf("environment '%s' already exists; inspect it with 'tt env show %s'", request.ID, request.ID))
	}
	count, err := s.db.VMCount(ctx)
	if err != nil {
		return nil, internal(err)
	}
	if count > model.MaxVMs || uint64(len(request.VMs)) > model.MaxVMs-count {
		return nil, apiError(409, "fleet VM limit reached")
	}
	hosts, err := s.db.ListHosts(ctx)
	if err != nil {
		return nil, internal(err)
	}
	for i := range hosts {
		host := &hosts[i]
		tracked := model.Resource{CPUTotal: host.Resource.CPUTotal, MemTotal: host.Resource.MemTotal, DiskTotal: host.Resource.DiskTotal}
		vms, err := s.db.VMsByHost(ctx, host.ID)
		if err != nil {
			return nil, internal(err)
		}
		for _, vm := range vms {
			tracked.Account(vm)
		}
		host.Resource.CPUUsed = max(host.Resource.CPUUsed, tracked.CPUUsed)
		host.Resource.MemUsed = max(host.Resource.MemUsed, tracked.MemUsed)
		host.Resource.DiskUsed = max(host.Resource.DiskUsed, tracked.DiskUsed)
	}
	images := HostImages{}
	for _, host := range hosts {
		names := map[string]bool{}
		for _, name := range host.Images {
			names[name] = true
		}
		images[host.ID] = names
	}
	placementSpecs := append([]api.VmSpec{}, request.VMs...)
	for index, spec := range placementSpecs {
		if spec.DurableRoot || spec.RootVolumeID != "" || len(spec.DataVolumeIDs) > 0 {
			var e error
			placementSpecs[index], e = s.prepareDiskPlacement(ctx, spec)
			if e != nil {
				return nil, internal(e)
			}
		}
	}
	placements, err := ScheduleEnv(hosts, placementSpecs, images)
	if err != nil {
		return nil, apiError(422, err.Error())
	}
	planned := []model.Vm{}
	requests := []creationRequest{}
	pendingRoots := []model.Volume{}
	for index, item := range placements {
		spec, placement := item.Spec, item.Placement
		keys := append(model.List[string]{}, spec.SSHKeys...)
		slices.Sort(keys)
		keys = slices.Compact(keys)
		id := uuid.NewString()
		cpu := defaultValue(spec.CPU, model.VMCPUDefault)
		mem := defaultValue(spec.Mem, model.VMMemDefault)
		requestedDisk := defaultValue(spec.Disk, spec.Engine.DefaultDisk())
		planned = append(planned, model.Vm{Backup: model.NewBackupState(), ID: id, EnvID: request.ID, HostID: placement.HostID, Image: spec.Image, Engine: spec.Engine, CPU: cpu, Mem: mem, Disk: placement.Disk, State: model.VmCreating, CreatedAt: created, Options: model.VmOptions{SSH: spec.SSH, Ports: spec.Ports, SSHKeys: keys, DenyOutgoing: spec.DenyOutgoing, RequestedDisk: requestedDisk, IsolatedNetwork: spec.IsolatedNetwork, GuestConfigDigest: guestconfig.Digest(spec.GuestConfig)}})
		requests = append(requests, creationRequest{placement.HostAddr, api.CreateVmReq{SSH: spec.SSH, VmID: id, EnvID: request.ID, Image: spec.Image, Engine: spec.Engine, CPU: cpu, Mem: mem, Disk: requestedDisk, Ports: spec.Ports, DenyOutgoing: spec.DenyOutgoing, SSHKeys: keys, IsolatedNetwork: spec.IsolatedNetwork, GuestConfig: spec.GuestConfig}})
		plannedVM := &planned[len(planned)-1]
		dispatch := &requests[len(requests)-1].request
		managed := spec.DurableRoot || spec.RootVolumeID != ""
		plannedVM.Options.RootManaged = managed
		plannedVM.Options.RootVolumeID = spec.RootVolumeID
		plannedVM.Options.InitialStopped = spec.InitialStopped
		dispatch.HostID = placement.HostID
		dispatch.VolumeOwner = request.Owner
		if dispatch.VolumeOwner == "" {
			dispatch.VolumeOwner = request.ID
		}
		dispatch.DurableRoot = spec.DurableRoot
		dispatch.RootRetain = spec.RootRetain
		dispatch.InitialStopped = spec.InitialStopped
		dispatch.DataVolumeIDs = spec.DataVolumeIDs
		if managed && spec.RootVolumeID == "" {
			rootID := managedRootID(request.ID, index, commandGeneration(ctx))
			existingRoot, e := s.db.GetVolume(ctx, rootID)
			if e != nil {
				return nil, internal(e)
			}
			if existingRoot != nil {
				return nil, diskAPIError("terminal", "volume_identity_exists", "a managed root identity cannot be reused; choose a new environment generation")
			}

			retain := true
			if spec.RootRetain != nil {
				retain = *spec.RootRetain
			}
			format := "raw"
			for _, host := range hosts {
				if host.ID == placement.HostID && host.Storage == model.File && spec.Engine == model.Qemu {
					format = "qcow2"
				}
			}
			pendingRoots = append(pendingRoots, model.Volume{ID: rootID, Owner: dispatch.VolumeOwner, HostID: placement.HostID, Generation: commandGeneration(ctx), SizeMiB: requestedDisk, Engine: spec.Engine, Image: spec.Image, Format: format, Retain: retain, State: "creating", Phase: "reserved", CreatedAt: created})
			plannedVM.Options.RootVolumeID = rootID
		}
		dispatch.RootVolumeID = plannedVM.Options.RootVolumeID

	}
	ids := model.List[string]{}
	for _, vm := range planned {
		ids = append(ids, vm.ID)
	}
	env := model.Env{ID: request.ID, Owner: request.Owner, VmIds: ids, CreatedAt: created, ExpiresAt: expires, State: model.EnvCreating}
	if managedRequest(request) {
		env.CreationHash = creationHash(request)
	}
	for i := range hosts {
		for _, vm := range planned {
			if vm.HostID == hosts[i].ID {
				hosts[i].Resource.Account(vm)
			}
		}
	}
	for i := range hosts {
		for _, root := range pendingRoots {
			if root.HostID == hosts[i].ID {
				hosts[i].Resource.DiskUsed = model.SatAdd(hosts[i].Resource.DiskUsed, root.ReservedMiB())
			}
		}
	}
	if err := s.db.transaction(ctx, "managed environment admission", func(tx *DB) error {
		if e := tx.PutEnv(ctx, &env); e != nil {
			return e
		}
		for i := range planned {
			if e := tx.PutVM(ctx, &planned[i]); e != nil {
				return e
			}
		}
		for i := range hosts {
			if e := tx.PutHost(ctx, &hosts[i]); e != nil {
				return e
			}
		}
		for i := range pendingRoots {
			if e := tx.PutVolume(ctx, &pendingRoots[i]); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	s.revision.Add(1)
	return requests, nil
}
func (s *Server) finishCreation(ctx context.Context, id string, requests []creationRequest) *APIError {
	reader, err := s.clientFactory(5 * time.Second)
	if err != nil {
		return internal(err)
	}
	client, err := s.clientFactory(360 * time.Second)
	if err != nil {
		return internal(err)
	}
	var effectError error
	for _, item := range requests {
		if envelope, ok := command.FromContext(ctx); ok {
			var highest uint64
			_, err := withDB(s, func(db *DB) (model.Unit, error) {
				e := db.q.QueryRowContext(ctx, "SELECT token FROM command_fences WHERE subject=?1", envelope.Subject).Scan(&highest)
				return model.Unit{}, e
			})
			if err != nil {
				return internal(err)
			}
			if highest > envelope.Token {
				return internal(&api.BoundaryError{Class: "state", Code: "stale_fence", Message: "creation ownership withdrawn"})
			}
		}
		env, err := withDB(s, func(db *DB) (*model.Env, error) { return db.GetEnv(ctx, id) })
		if err != nil {
			return internal(err)
		}
		if env == nil || (env.ExpiresAt != 0 && env.ExpiresAt <= s.now()) {
			break
		}
		vm, err := client.create(ctx, item.address, item.request)
		if ctx.Err() != nil {
			return internal(ctx.Err())
		}
		if err == nil {
			if _, err := withDB(s, func(db *DB) (model.Unit, error) { return model.Unit{}, db.PutVM(ctx, vm) }); err != nil {
				return internal(err)
			}
		} else {
			effectError = err
			message := err.Error()
			_, err := withDB(s, func(db *DB) (model.Unit, error) {
				vm, err := db.GetVM(ctx, item.request.VmID)
				if err != nil {
					return model.Unit{}, err
				}
				if vm == nil {
					return model.Unit{}, fmt.Errorf("missing planned VM")
				}
				vm.Error = &message
				return model.Unit{}, db.PutVM(ctx, vm)
			})
			if err != nil {
				return internal(err)
			}
			s.log("[ctl] VM %s: %s", item.request.VmID, message)
		}
	}
	s.refreshHosts(ctx, reader, &id)
	if err := s.updateEnvState(ctx, id); err != nil {
		return internal(err)
	}
	if effectError != nil {
		return diskAPIError("unknown", "creation_incomplete", "pinned creation remains retained for idempotent replay")
	}
	return nil
}
func (s *Server) deleteEnvironment(ctx context.Context, id string, expiryOnly bool) *APIError {
	operation, apiErr := s.beginOperation(id)
	if apiErr != nil {
		return apiErr
	}
	defer operation.Close()
	s.dbMu.Lock()
	env, err := s.db.GetEnv(ctx, id)
	if err != nil {
		s.dbMu.Unlock()
		return internal(err)
	}
	if env == nil {
		s.dbMu.Unlock()
		return nil
	}
	if expiryOnly && env.State != model.EnvDeleting && (env.ExpiresAt == 0 || env.ExpiresAt > s.now()) {
		s.dbMu.Unlock()
		return nil
	}
	env.State = model.EnvDeleting
	if err := s.db.PutEnv(ctx, env); err != nil {
		s.dbMu.Unlock()
		return internal(err)
	}
	vms, err := s.db.VMsByEnv(ctx, id)
	if err != nil {
		s.dbMu.Unlock()
		return internal(err)
	}
	hosts, _, err := s.db.RecoveryHosts(ctx)
	s.dbMu.Unlock()
	if err != nil {
		return internal(err)
	}
	client, err := s.clientFactory(360 * time.Second)
	if err != nil {
		return internal(err)
	}
	failures := []string{}
	var boundaryFailure *api.BoundaryError
	for _, vm := range vms {
		host := findHost(hosts, vm.HostID)
		var err error
		if host == nil {
			err = fmt.Errorf("host %s is missing", vm.HostID)
		} else if host.State != model.HostOnline {
			err = fmt.Errorf("host %s is offline; cleanup retained for retry", vm.HostID)
		} else {
			err = client.action(ctx, *host, vm.ID, nil)
		}
		if ctx.Err() != nil {
			return internal(ctx.Err())
		}
		if err == nil {
			// Confirmed deletion returns the reservation immediately instead of
			// charging the host for up to one refresh cycle.
			if _, err := withDB(s, func(db *DB) (model.Unit, error) { return model.Unit{}, db.RemoveVMRelease(ctx, vm) }); err != nil {
				return internal(err)
			}
		} else {
			failures = append(failures, vm.ID+": "+err.Error())
			var typed *api.BoundaryError
			if errors.As(err, &typed) {
				boundaryFailure = typed
			}
		}
	}
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	if len(failures) == 0 {
		return internal(s.db.RemoveEnv(ctx, id))
	}
	message := strings.Join(failures, "; ")
	env.Error = &message
	remaining, err := s.db.VMsByEnv(ctx, id)
	if err != nil {
		return internal(err)
	}
	env.VmIds = model.List[string]{}
	for _, vm := range remaining {
		env.VmIds = append(env.VmIds, vm.ID)
	}
	if err := s.db.PutEnv(ctx, env); err != nil {
		return internal(err)
	}
	if boundaryFailure != nil {
		return internal(boundaryFailure)
	}
	return apiError(502, "cleanup incomplete; environment retained for retry: "+message)
}
func findHost(hosts []model.Host, id string) *model.Host {
	for i := range hosts {
		if hosts[i].ID == id {
			return &hosts[i]
		}
	}
	return nil
}
func (s *Server) changeEnvironment(ctx context.Context, id, action string, options ...model.StopOptions) *APIError {
	operation, apiErr := s.beginOperation(id)
	if apiErr != nil {
		return apiErr
	}
	defer operation.Close()
	detail, apiErr := s.environmentDetail(ctx, id)
	if apiErr != nil {
		return apiErr
	}
	for _, vm := range detail.VMs {
		if vm.Backup.Busy() {
			return apiError(409, "backup outcome unresolved; inspect or retry that operation before lifecycle changes")
		}
	}
	if detail.Env.State == model.EnvCreating || detail.Env.State == model.EnvDeleting {
		return apiError(409, "environment has an operation in progress")
	}
	hosts, err := withDB(s, func(db *DB) ([]model.Host, error) {
		hosts, _, err := db.RecoveryHosts(ctx)
		return hosts, err
	})
	if err != nil {
		return internal(err)
	}
	client, err := s.clientFactory(360 * time.Second)
	if err != nil {
		return internal(err)
	}
	if action == "start" {
		for _, vm := range detail.VMs {
			if vm.PendingResources != nil {
				return apiError(409, "resource update unfinished; retry the recorded resources before starting")
			}
		}
		starting := append([]model.Vm{}, detail.VMs...)
		for i := range starting {
			if starting[i].State == model.VmStopped {
				starting[i].State = model.VmCreating
			}
		}
		if _, err := withDB(s, func(db *DB) (model.Unit, error) {
			if err := db.PutEnvironment(ctx, &detail.Env, starting, nil); err != nil {
				return model.Unit{}, err
			}
			s.revision.Add(1)
			return model.Unit{}, nil
		}); err != nil {
			return internal(err)
		}
	}
	failures := []string{}
	var boundaryFailure *api.BoundaryError
	for _, vm := range detail.VMs {
		host := findHost(hosts, vm.HostID)
		var err error
		if host == nil {
			err = fmt.Errorf("host %s is missing", vm.HostID)
		} else {
			err = client.action(ctx, *host, vm.ID, &action, options...)
		}
		if err == nil {
			confirmed := vm
			confirmed.State = model.VmStopped
			if action == "start" {
				confirmed.State = model.VmRunning
			}
			confirmed.Error = nil
			if _, err := withDB(s, func(db *DB) (model.Unit, error) { return model.Unit{}, db.PutVM(ctx, &confirmed) }); err != nil {
				return internal(err)
			}
		} else {
			failures = append(failures, vm.ID+": "+err.Error())
			var typed *api.BoundaryError
			if errors.As(err, &typed) {
				boundaryFailure = typed
			}
		}
	}
	reader, err := s.clientFactory(5 * time.Second)
	if err != nil {
		return internal(err)
	}
	s.refreshHosts(ctx, reader, &id)
	if err := s.updateEnvState(ctx, id); err != nil {
		return internal(err)
	}
	env, err := withDB(s, func(db *DB) (*model.Env, error) { return db.GetEnv(ctx, id) })
	if err != nil {
		return internal(err)
	}
	if env == nil {
		return internal(fmt.Errorf("environment disappeared"))
	}
	expected := model.EnvStopped
	if action == "start" {
		expected = model.EnvActive
	}
	if len(failures) > 0 || env.State != expected {
		env.State = model.EnvFailed
		message := action + " incomplete: " + strings.Join(failures, "; ")
		if len(failures) == 0 {
			message = action + " incomplete: the resulting state did not match the request"
		}
		env.Error = &message
		if _, err := withDB(s, func(db *DB) (model.Unit, error) { return model.Unit{}, db.PutEnv(ctx, env) }); err != nil {
			return internal(err)
		}
		if boundaryFailure != nil {
			return internal(boundaryFailure)
		}
		return apiError(502, message)
	}
	return nil
}
