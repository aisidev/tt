package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/internal/engine"
	"github.com/aisidev/tt/internal/storage"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

func (r *Runtime) processScope(ctx context.Context) (engine.ProcessScope, error) {
	scope := engine.ProcessScope{Cgroup: r.processDomain, Roots: []string{r.runDir, filepath.Dir(r.archiveDatabasePath(ctx))}}
	if r.processDomain != "" {
		boot, err := engine.KernelBootIdentity()
		if err != nil {
			return scope, err
		}
		scope.KernelBootID = boot
	}
	if r.storage == model.Zvol {
		directory, err := storage.NamespaceDirectory(ctx, r.store, r.runtimeDir)
		if err != nil {
			return scope, err
		}
		scope.Roots = append(scope.Roots, directory)
		imageDir, err := storage.NamespaceDirectory(ctx, r.store, r.imageDir)
		if err != nil {
			return scope, err
		}
		scope.Roots = append(scope.Roots, imageDir)
		scope.Dataset = r.runtimeDir
	} else {
		scope.Roots = append(scope.Roots, r.runtimeDir, r.imageDir)
	}
	return scope, nil
}
func (s *Server) controlHTTP(w http.ResponseWriter, r *http.Request) {
	report, err := s.commandJournal.ControlReport()
	if err != nil {
		failure(w, err)
		return
	}
	if report.Enabled {
		scope, err := s.runtime.processScope(r.Context())
		if err != nil || s.runtime.processDomain == "" || runtime.GOOS != "linux" {
			failure(w, diskError("unknown", "scope_unavailable", "physical process scope is unavailable"))
			return
		}
		raw, _ := json.Marshal(scope)
		report.ScopeDigest = fmt.Sprintf("%x", sha256.Sum256(raw))
		report.Engines = []model.Engine{model.Qemu, model.Firecracker}
	}
	api.WriteJSON(w, 200, api.Success(report))
}
func (s *Server) fenceControlHTTP(w http.ResponseWriter, request *http.Request) {
	envelope, ok := command.FromContext(request.Context())
	if !ok || s.commandJournal.Authority == nil {
		failure(w, diskError("terminal", "missing_identity", "receiver fencing requires an external epoch identity"))
		return
	}
	if runtime.GOOS != "linux" || s.runtime.processDomain == "" {
		failure(w, diskError("terminal", "unimplemented", "physical epoch fencing is unsupported on this platform"))
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Minute)
	defer cancel()
	authority := s.commandJournal.Authority
	current := func() error {
		state, err := authority.State()
		if err != nil {
			return err
		}
		if state.ID != envelope.Authority || state.Epoch != envelope.Epoch || (state.Phase != "fenced" && state.Phase != "inventory") {
			return diskError("state", "stale_epoch", "fencing ownership changed")
		}
		return nil
	}
	if existing, err := authority.FenceReceipt(); err == nil && existing != nil && existing.Key == envelope.Key && existing.HostID == s.runtime.hostID {
		api.WriteJSON(w, 200, api.Success(*existing))
		return
	}
	for !s.commandJournal.IdleExcept(envelope.Subject) {
		if err := current(); err != nil {
			failure(w, err)
			return
		}
		select {
		case <-ctx.Done():
			failure(w, diskError("unknown", "fence_unconfirmed", "earlier receiver commands have not drained"))
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	// This lock drains any previously admitted local reconciliation. Its loop
	// rechecks the gate inside the same lock, so it cannot queue a later restart.
	s.mutation.Lock()
	defer s.mutation.Unlock()
	if err := current(); err != nil {
		failure(w, err)
		return
	}
	if len(s.runtime.transferActive) > 0 {
		failure(w, diskError("unknown", "fence_unconfirmed", "an earlier storage transfer is still active"))
		return
	}
	vms, corrupt, err := loadRecoverableVMs(ctx, s.runtime.db.conn, s.runtime.db.stderr)
	if err != nil || corrupt {
		failure(w, diskError("unknown", "fence_unconfirmed", "VM ownership is not readable"))
		return
	}
	for _, vm := range vms {
		if vm.Engine != model.Qemu && vm.Engine != model.Firecracker {
			failure(w, diskError("terminal", "unimplemented", "this workload engine requires a separate physical fencing verifier"))
			return
		}
	}
	scope, err := s.runtime.processScope(ctx)
	if err != nil {
		failure(w, diskError("unknown", "fence_unconfirmed", "owned process namespace is unavailable"))
		return
	}
	first := []engine.ProcessEvidence{}
	for round := 0; round < 4; round++ {
		if err = current(); err != nil {
			failure(w, err)
			return
		}
		processes, err := engine.ScopedProcesses(ctx, scope)
		if err != nil {
			failure(w, diskError("unknown", "fence_unconfirmed", "owned process inventory unavailable"))
			return
		}
		if round == 0 {
			first = processes
		}
		if len(processes) == 0 {
			break
		}
		if round == 3 {
			failure(w, diskError("unknown", "fence_unconfirmed", "owned processes remain after fencing"))
			return
		}
		for _, process := range processes {
			if err = current(); err != nil {
				failure(w, err)
				return
			}
			if err = engine.StopScopedProcess(ctx, process, scope); err != nil {
				failure(w, diskError("unknown", "fence_unconfirmed", "owned process exit was not confirmed"))
				return
			}
		}
	}
	for index := range vms {
		vm := &vms[index]
		implementation, err := s.runtime.engineFactory(vm.Engine, s.runtime.containerRuntime)
		if err != nil {
			failure(w, diskError("unknown", "fence_unconfirmed", "VM executor state unavailable"))
			return
		}
		state, err := implementation.State(ctx, vm)
		if err != nil || state != model.VmStopped {
			failure(w, diskError("unknown", "fence_unconfirmed", "VM process absence was not confirmed"))
			return
		}
		vm.State = model.VmStopped
		vm.Error = nil
		if vm.SSH != nil {
			vm.SSH.Ready = false
		}
		if err = saveVM(ctx, s.runtime.db.conn, vm); err != nil {
			failure(w, diskError("unknown", "fence_unconfirmed", "fenced observation could not be persisted"))
			return
		}
	}
	if err = s.runtime.recount(ctx); err != nil {
		failure(w, diskError("unknown", "fence_unconfirmed", "fenced resource accounting unavailable"))
		return
	}
	if err = current(); err != nil {
		failure(w, err)
		return
	}
	scopeBytes, _ := json.Marshal(scope)
	processBytes, _ := json.Marshal(first)
	receipt := model.FencingReceipt{Engines: []model.Engine{model.Qemu, model.Firecracker}, Version: 1, ID: uuid.NewString(), Authority: envelope.Authority, Epoch: envelope.Epoch, HostID: s.runtime.hostID, Key: envelope.Key, Method: "owned_processes_stopped", ScopeDigest: fmt.Sprintf("%x", sha256.Sum256(scopeBytes)), ProcessDigest: fmt.Sprintf("%x", sha256.Sum256(processBytes)), KnownVMs: len(vms), CompletedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = authority.CommitFence(receipt); err != nil {
		failure(w, diskError("unknown", "fence_unconfirmed", "external fencing receipt could not be committed"))
		return
	}
	s.runtime.observationUnsafe.Store(false)
	api.WriteJSON(w, 200, api.Success(receipt))
}
