package ctl

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/aisidev/tt/internal/backup"
	"github.com/aisidev/tt/model"
)

func (s *Server) RefreshAllHosts(ctx context.Context, client *agentClient) {
	s.refreshHosts(ctx, client, nil)
}
func (s *Server) refreshHosts(ctx context.Context, client *agentClient, ownedEnv *string) {
	revision := s.revision.Load()
	hosts, err := withDB(s, func(db *DB) ([]model.Host, error) {
		hosts, corrupt, err := db.RecoveryHosts(ctx)
		if err != nil {
			return nil, err
		}
		for _, id := range corrupt {
			if s.commandJournal.Authority != nil {
				return nil, fmt.Errorf("recovery host catalog is unreadable")
			}
			// A row the decoder cannot read would otherwise stay a phantom
			// online host forever: online to the scheduler, unreachable to
			// this loop. Replace it with a visible offline marker; the fresh
			// snapshot pass below then converges it like any offline host.
			// The storage field cannot express "unknown", and an offline host
			// is never scheduled onto, so the placeholder never takes effect.
			message := "unreadable host record replaced by an offline marker; re-register the agent"
			if err := db.PutHost(ctx, &model.Host{ID: id, State: model.HostOffline, Engines: model.List[model.Engine]{}, Images: model.List[string]{}, Storage: model.File, Error: &message}); err != nil {
				return nil, err
			}
		}
		if ownedEnv != nil {
			vms, err := db.VMsByEnv(ctx, *ownedEnv)
			if err != nil {
				return nil, err
			}
			ids := map[string]bool{}
			for _, vm := range vms {
				ids[vm.HostID] = true
			}
			filtered := []model.Host{}
			for _, host := range hosts {
				if ids[host.ID] {
					filtered = append(filtered, host)
				}
			}
			hosts = filtered
		}
		return hosts, nil
	})
	if err != nil {
		s.log("[ctl] list hosts: %v", err)
		return
	}
	for start := 0; start < len(hosts); start += 8 {
		batch := hosts[start:min(start+8, len(hosts))]
		type result struct {
			id       string
			snapshot hostSnapshot
		}
		results := make(chan result, len(batch))
		var workers sync.WaitGroup
		for _, host := range batch {
			workers.Add(1)
			go func(host model.Host) { defer workers.Done(); results <- result{host.ID, client.snapshot(ctx, host)} }(host)
		}
		go func() { workers.Wait(); close(results) }()
		for item := range results {
			if err := s.applySnapshot(ctx, item.id, item.snapshot, &revision, ownedEnv); err != nil {
				s.log("[ctl] save snapshot: %v", err)
			}
		}
	}
	envs, err := withDB(s, func(db *DB) ([]model.Env, error) {
		envs, corrupt, err := db.RecoveryEnvs(ctx)
		if err != nil {
			return nil, err
		}
		for _, id := range corrupt {
			if s.commandJournal.Authority != nil {
				return nil, fmt.Errorf("recovery environment catalog is unreadable")
			}
			message := "unreadable environment record replaced by a failed marker; delete and recreate the environment"
			if err := db.PutEnv(ctx, &model.Env{ID: id, State: model.EnvFailed, VmIds: model.List[string]{}, Error: &message}); err != nil {
				return nil, err
			}
		}
		return envs, nil
	})
	if err != nil {
		s.log("[ctl] list envs: %v", err)
		return
	}
	for _, env := range envs {
		if (ownedEnv == nil || *ownedEnv != env.ID) && s.busy(env.ID) {
			continue
		}
		if err := s.updateEnvState(ctx, env.ID); err != nil {
			s.log("[ctl] update %s: %v", env.ID, err)
		}
	}
}
func (s *Server) applySnapshot(ctx context.Context, id string, snapshot hostSnapshot, revision *uint64, ownedEnv *string) error {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	if revision != nil && *revision != s.revision.Load() {
		return nil
	}
	host, err := s.db.GetHost(ctx, id)
	if err != nil || host == nil {
		return err
	}
	if snapshot.err != nil {
		message := snapshot.err.Error()
		host.Error = &message
		host.State = model.HostOffline
		s.log("[ctl] host %s: %s", id, message)
		return s.db.PutHost(ctx, host)
	}
	info, actual := snapshot.info, snapshot.vms
	host.Resource = model.Resource{CPUTotal: info.Resource.CPUTotal, MemTotal: info.Resource.MemTotal, DiskTotal: info.Resource.DiskTotal}
	host.Engines = info.Engines
	host.Capabilities = info.Capabilities
	host.CapabilityReport = info.CapabilityReport
	host.StorageFeatures = info.StorageFeatures
	host.Storage = info.Storage
	host.Images = info.Images
	host.ImageSizes = info.ImageSizes
	host.State = model.HostOnline
	host.Error = nil
	if len(info.Warnings) > 0 {
		message := strings.Join(info.Warnings, "; ")
		host.Error = &message
	}
	known, err := s.db.VMsByHost(ctx, id)
	if err != nil {
		return err
	}
	for _, vm := range actual {
		tracked := false
		for _, old := range known {
			tracked = tracked || old.ID == vm.ID
		}
		if !tracked {
			s.log("[ctl] host %s: untracked VM %s (environment %s); inspect agent inventory", id, vm.ID, vm.EnvID)
			host.Resource.Account(vm)
		}
	}
	for _, old := range known {
		if (ownedEnv == nil || *ownedEnv != old.EnvID) && s.busy(old.EnvID) {
			host.Resource.Account(old)
			continue
		}
		var observed *model.Vm
		for i := range actual {
			vm := &actual[i]
			if vm.ID == old.ID && vm.EnvID == old.EnvID && vm.HostID == id {
				observed = vm
				break
			}
		}
		if observed != nil {
			merged := mergeVMSnapshot(old, *observed)
			host.Resource.Account(merged)
			if err := s.db.PutVM(ctx, &merged); err != nil {
				return err
			}
		} else {
			// Absence in one inventory cannot confirm deletion or release reservations.
			old.State = model.VmFailed
			if old.Error == nil {
				message := "VM absent from agent snapshot; inspect or delete to reconcile"
				old.Error = &message
			}
			host.Resource.Account(old)
			if err := s.db.PutVM(ctx, &old); err != nil {
				return err
			}
		}
	}
	host.Resource.CPUUsed = max(host.Resource.CPUUsed, info.Resource.CPUUsed)
	host.Resource.MemUsed = max(host.Resource.MemUsed, info.Resource.MemUsed)
	host.Resource.DiskUsed = max(host.Resource.DiskUsed, info.Resource.DiskUsed)
	pending, err := s.db.pendingDiskReservation(ctx, id)
	if err != nil {
		return err
	}
	host.Resource.DiskUsed = model.SatAdd(host.Resource.DiskUsed, pending)
	return s.db.PutHost(ctx, host)
}

// An older snapshot cannot cancel an operation still queued at the agent. Keep
// resource and backup intent until a matching pending/result/target confirms it.
func mergeVMSnapshot(known, actual model.Vm) model.Vm {
	vm := actual
	if known.Backup.Sequence > actual.Backup.Sequence || (known.Backup.Sequence == actual.Backup.Sequence && known.Backup.LastResult != nil && actual.Backup.Pending != nil && backup.Equal(known.Backup.LastResult.Request, actual.Backup.Pending.Request)) {
		vm.Backup = known.Backup
		vm.SSH = known.SSH
	}
	if request := known.Backup.Forwarding; request != nil {
		confirmed := (actual.Backup.Pending != nil && backup.Equal(actual.Backup.Pending.Request, *request)) || (actual.Backup.LastResult != nil && backup.Equal(actual.Backup.LastResult.Request, *request)) || (known.Backup.ForwardingSettledSequence != nil && actual.Backup.Sequence >= *known.Backup.ForwardingSettledSequence)
		if !confirmed {
			vm.Backup.Forwarding = request
			vm.Backup.ForwardingReservedMiB = known.Backup.ForwardingReservedMiB
			vm.Backup.ForwardingSettledSequence = known.Backup.ForwardingSettledSequence
		} else {
			vm.Backup.Forwarding = nil
			vm.Backup.ForwardingReservedMiB = 0
			vm.Backup.ForwardingSettledSequence = nil
		}
	}
	if target := known.PendingResources; target != nil {
		overhead := vm.Options.ConfigDiskMiB(vm.Engine)
		completed := vm.PendingResources == nil && vm.CPU == target.CPU && vm.Mem == target.Mem && target.Disk <= math.MaxUint32-overhead && target.Disk+overhead == vm.Disk && vm.Options.RequestedDisk == target.Disk
		if vm.PendingResources == nil && !completed {
			copy := *target
			vm.PendingResources = &copy
			if vm.Error == nil {
				message := "resource update outcome unknown; retry the recorded target"
				vm.Error = &message
			}
		}
	}
	return vm
}
func (s *Server) updateEnvState(ctx context.Context, id string) error {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	env, err := s.db.GetEnv(ctx, id)
	if err != nil || env == nil {
		return err
	}
	if env.State == model.EnvDeleting {
		return nil
	}
	vms, err := s.db.VMsByEnv(ctx, id)
	if err != nil {
		return err
	}
	hosts, _, err := s.db.RecoveryHosts(ctx)
	if err != nil {
		return err
	}
	for _, vm := range vms {
		online := false
		for _, host := range hosts {
			online = online || (host.ID == vm.HostID && host.State == model.HostOnline)
		}
		if !online {
			env.State = model.EnvFailed
			message := "one or more agents are offline; VM states are last observed snapshots"
			env.Error = &message
			return s.db.PutEnv(ctx, env)
		}
	}
	hasError, creating, allRunning, allStopped := false, false, true, true
	for _, vm := range vms {
		hasError = hasError || vm.Error != nil
		creating = creating || vm.State == model.VmCreating
		allRunning = allRunning && vm.State == model.VmRunning
		allStopped = allStopped && vm.State == model.VmStopped
	}
	switch {
	case len(vms) == 0 || hasError:
		env.State = model.EnvFailed
	case creating:
		env.State = model.EnvCreating
	case allRunning:
		env.State = model.EnvActive
	case allStopped:
		env.State = model.EnvStopped
	default:
		env.State = model.EnvFailed
	}
	env.VmIds = model.List[string]{}
	for _, vm := range vms {
		env.VmIds = append(env.VmIds, vm.ID)
	}
	if env.State == model.EnvFailed && len(vms) == 0 {
		if env.Error == nil {
			message := "environment has no tracked VMs"
			env.Error = &message
		}
	} else if env.State == model.EnvFailed {
		messages := []string{}
		for _, vm := range vms {
			message := string(vm.State)
			if vm.Error != nil {
				message = *vm.Error
			}
			messages = append(messages, vm.ID+": "+message)
		}
		message := strings.Join(messages, "; ")
		env.Error = &message
	} else {
		env.Error = nil
	}
	return s.db.PutEnv(ctx, env)
}
