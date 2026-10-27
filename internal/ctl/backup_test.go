package ctl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/backup"
	"github.com/aisidev/tt/internal/capability"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

type backupPeer struct {
	mu                      sync.Mutex
	view                    api.BackupView
	writes                  int
	reject, wrongSettlement bool
}

func (p *backupPeer) get(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	api.WriteJSON(w, 200, api.Success(p.view))
}
func (p *backupPeer) restore(w http.ResponseWriter, r *http.Request) {
	body, ok := api.ReadJSON[api.RestoreBody](w, r)
	if !ok {
		return
	}
	request, err := backup.FromHeaders(api.HeaderText(r.Header, "Idempotency-Key"), api.HeaderText(r.Header, "If-Match"), model.BackupRestore, &body.Generation)
	if err != nil {
		api.WriteJSON(w, 400, api.Failure[model.Unit](err.Error()))
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reject {
		operation := request.OperationID
		if p.wrongSettlement {
			operation = uuid.NewString()
		}
		w.Header().Set("x-tt-backup-settled", operation)
		w.Header().Set("x-tt-backup-settled-sequence", "0")
		api.WriteJSON(w, 409, api.Failure[model.Unit]("conflict: injected admission rejection"))
		return
	}
	completed, err := backup.Check(p.view.Vm.Backup, request)
	if err != nil || completed {
		api.WriteJSON(w, 409, api.Failure[model.Unit]("unexpected repeated writer"))
		return
	}
	p.writes++
	p.view.Vm.Backup.Sequence++
	p.view.Vm.Backup.Revision = uuid.NewString()
	p.view.Vm.Backup.LastResult = &model.BackupReceipt{Request: request, CompletedAt: 1}
	api.WriteJSON(w, 502, api.Failure[model.Unit]("injected response loss after disk write"))
}
func backupFixture(t *testing.T) (*Server, *backupPeer, model.BackupRequest) {
	t.Helper()
	db := testDB(t)
	s := newServer(db, nil)
	vm := vmRecord("vm", "env", "host")
	vm.CPU = 1
	vm.Mem = 128
	vm.Disk = 16
	vm.State = model.VmStopped
	vm.Backup.Current = &model.BackupGeneration{ID: uuid.NewString(), Backend: model.BackupReflink, RootBytes: 16 * 1024 * 1024, Identity: "file", Dependencies: "boot"}
	peer := &backupPeer{view: api.BackupView{Vm: vm, Enabled: true, Supported: true}}
	httpPeer := httptest.NewServer(api.Router([]api.Route{{Pattern: "/api/vms/{id}/backup", Methods: []string{"GET"}, Handler: peer.get}, {Pattern: "/api/vms/{id}/backup/restore", Methods: []string{"POST"}, Handler: peer.restore}}))
	t.Cleanup(httpPeer.Close)
	host := hostRecord("host")
	host.Addr = strings.TrimPrefix(httpPeer.URL, "http://")
	host.Capabilities = append(model.List[string]{}, capability.LegacyAll...)
	host.Resource.DiskUsed = vm.ReservedDisk()
	env := envRecord("env")
	env.VmIds = model.List[string]{vm.ID}
	env.State = model.EnvStopped
	if err := db.PutHost(context.Background(), &host); err != nil {
		t.Fatal(err)
	}
	if err := db.PutEnvironment(context.Background(), &env, []model.Vm{vm}, nil); err != nil {
		t.Fatal(err)
	}
	request := model.BackupRequest{OperationID: uuid.NewString(), ExpectedRevision: vm.Backup.Revision, Action: model.BackupRestore, Generation: &vm.Backup.Current.ID}
	return s, peer, request
}
func backupHeaders(request model.BackupRequest) http.Header {
	headers := http.Header{}
	headers.Set("Idempotency-Key", request.OperationID)
	headers.Set("If-Match", "\""+request.ExpectedRevision+"\"")
	return headers
}
func TestForwardedRestoreLostReplyReplayAndHeadersAreSafe(t *testing.T) {
	s, peer, request := backupFixture(t)
	ctx := context.Background()
	if _, err := s.backupMutation(ctx, "vm", backupHeaders(request), model.BackupRestore, request.Generation); err == nil || err.Status != 502 {
		t.Fatal(err)
	}
	peer.mu.Lock()
	writes := peer.writes
	peer.mu.Unlock()
	if writes != 1 {
		t.Fatal(writes)
	}
	stored, err := s.db.GetVM(ctx, "vm")
	if err != nil || stored.Backup.Forwarding != nil || stored.Backup.LastResult == nil || !backup.Equal(stored.Backup.LastResult.Request, request) {
		t.Fatal(stored, err)
	}
	replay, apiErr := s.backupMutation(ctx, "vm", backupHeaders(request), model.BackupRestore, request.Generation)
	if apiErr != nil || replay.Vm.Backup.LastResult == nil {
		t.Fatal(replay, apiErr)
	}
	peer.mu.Lock()
	writes = peer.writes
	peer.mu.Unlock()
	if writes != 1 {
		t.Fatal("replayed disk write", writes)
	}
	different := uuid.NewString()
	if _, err := s.backupMutation(ctx, "vm", backupHeaders(request), model.BackupRestore, &different); err == nil || err.Status != 409 {
		t.Fatal(err)
	}
}
func TestForwardedRejectionReleasesOnlyWithMatchingSettledHeader(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		s, peer, request := backupFixture(t)
		peer.mu.Lock()
		peer.reject = true
		peer.wrongSettlement = wrong
		peer.mu.Unlock()
		ctx := context.Background()
		if _, err := s.backupMutation(ctx, "vm", backupHeaders(request), model.BackupRestore, request.Generation); err == nil || err.Status != 409 {
			t.Fatal(err)
		}
		vm, err := s.db.GetVM(ctx, "vm")
		if err != nil {
			t.Fatal(err)
		}
		host, err := s.db.GetHost(ctx, "host")
		if err != nil {
			t.Fatal(err)
		}
		if wrong {
			if vm.Backup.Forwarding == nil || vm.Backup.ForwardingReservedMiB != 16 || host.Resource.DiskUsed != 48 {
				t.Fatal(vm, host)
			}
		} else {
			if vm.Backup.Forwarding != nil || vm.Backup.ForwardingReservedMiB != 0 || host.Resource.DiskUsed != 32 {
				t.Fatal(vm, host)
			}
		}
	}
}
func TestOldObservationsCannotEraseIntentOrRestoreCompletion(t *testing.T) {
	original := vmRecord("vm", "env", "host")
	original.State = model.VmStopped
	original.Disk = 16
	original.CPU = 1
	original.Mem = 128
	request := model.BackupRequest{OperationID: uuid.NewString(), ExpectedRevision: original.Backup.Revision, Action: model.BackupCreate}
	known := original
	known.Backup.Forwarding = &request
	known.Backup.ForwardingReservedMiB = 16
	merged := mergeVMSnapshot(known, original)
	if merged.Backup.Forwarding == nil || merged.ReservedDisk() != 32 {
		t.Fatal(merged)
	}
	settled := uint64(1)
	known.Backup.ForwardingSettledSequence = &settled
	merged = mergeVMSnapshot(known, original)
	if merged.Backup.Forwarding == nil {
		t.Fatal("stale snapshot released reservation")
	}
	actual := original
	actual.Backup.Sequence = 1
	merged = mergeVMSnapshot(known, actual)
	if merged.Backup.Forwarding != nil || merged.Backup.ForwardingReservedMiB != 0 {
		t.Fatal(merged)
	}
	known = original
	known.Backup.Sequence = 2
	known.Backup.LastResult = &model.BackupReceipt{Request: request, CompletedAt: 10}
	known.SSH = &model.SshInfo{Ready: true, Initialized: true, CheckedAt: 20}
	actual = original
	actual.Backup.Sequence = 2
	actual.Backup.Pending = &model.BackupPending{Request: request}
	actual.SSH = &model.SshInfo{Ready: false, Initialized: false}
	merged = mergeVMSnapshot(known, actual)
	if merged.Backup.Pending != nil || merged.Backup.LastResult == nil || merged.SSH == nil || !merged.SSH.Ready || merged.SSH.CheckedAt != 20 {
		t.Fatal(merged)
	}
	target := model.VmResources{CPU: 2, Mem: 256, Disk: 32}
	known = original
	known.PendingResources = &target
	merged = mergeVMSnapshot(known, original)
	if merged.PendingResources == nil || *merged.PendingResources != target || merged.Error == nil {
		t.Fatal(merged)
	}
	actual = original
	actual.CPU = 2
	actual.Mem = 256
	actual.Disk = 32
	actual.Options.RequestedDisk = 32
	merged = mergeVMSnapshot(known, actual)
	if merged.PendingResources != nil {
		t.Fatal("confirmed target retained as pending", merged)
	}
}
