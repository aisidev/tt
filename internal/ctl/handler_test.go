package ctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/capability"
	"github.com/aisidev/tt/internal/guestconfig"
	"github.com/aisidev/tt/model"
)

type mockAgent struct {
	mu           sync.Mutex
	vms          []model.Vm
	caps         []string
	resource     model.Resource
	failDelete   bool
	failAction   bool
	rejectResize bool
	loseCreate   bool
	loseResize   bool
	creates      atomic.Int32
	resizes      atomic.Int32
	beforeInfo   func()
	beforeStart  func()
	beforeDelete func()
	beforeCreate func(api.CreateVmReq)
	beforeResize func()
}

func (m *mockAgent) handler() http.Handler {
	return api.Router([]api.Route{
		{Pattern: "/api/info", Methods: []string{"GET"}, Handler: m.infoHTTP},
		{Pattern: "/api/vms", Methods: []string{"GET", "POST"}, Handler: m.vmsHTTP},
		{Pattern: "/api/vms/{id}", Methods: []string{"DELETE"}, Handler: m.deleteHTTP},
		{Pattern: "/api/vms/{id}/start", Methods: []string{"POST"}, Handler: func(w http.ResponseWriter, r *http.Request) { m.actionHTTP(w, r, true) }},
		{Pattern: "/api/vms/{id}/stop", Methods: []string{"POST"}, Handler: func(w http.ResponseWriter, r *http.Request) { m.actionHTTP(w, r, false) }},
		{Pattern: "/api/vms/{id}/resources", Methods: []string{"POST"}, Handler: m.resizeHTTP},
	})
}
func (m *mockAgent) infoHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	before := m.beforeInfo
	m.mu.Unlock()
	if before != nil {
		before()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	resource := m.resource
	resource.CPUUsed = 0
	resource.MemUsed = 0
	resource.DiskUsed = 0
	resource.VmCount = 0
	for _, vm := range m.vms {
		resource.Account(vm)
	}
	engines := model.List[model.Engine]{model.Qemu, model.Firecracker}
	report := capability.FromProbes(model.File, engines, m.caps, true, nil)
	api.WriteJSON(w, 200, api.Success(api.AgentInfo{HostID: "host", Resource: resource, Storage: model.File, Engines: engines, Images: model.List[string]{"ubuntu"}, ImageSizes: model.Map[string, uint32]{"ubuntu": 16}, Capabilities: m.caps, CapabilityReport: &report}))
}
func (m *mockAgent) vmsHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		m.mu.Lock()
		defer m.mu.Unlock()
		api.WriteJSON(w, 200, api.Success(model.List[model.Vm](m.vms)))
		return
	}
	request, ok := api.ReadJSON[api.CreateVmReq](w, r)
	if !ok {
		return
	}
	m.mu.Lock()
	before := m.beforeCreate
	m.mu.Unlock()
	if before != nil {
		before(request)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creates.Add(1)
	options := model.VmOptions{SSH: request.SSH, Ports: request.Ports, SSHKeys: request.SSHKeys, DenyOutgoing: request.DenyOutgoing, RequestedDisk: request.Disk, IsolatedNetwork: request.IsolatedNetwork, GuestConfigDigest: guestconfig.Digest(request.GuestConfig)}
	vm := vmRecord(request.VmID, request.EnvID, "host")
	vm.Engine = request.Engine
	vm.CPU = request.CPU
	vm.Mem = request.Mem
	vm.Disk = max(request.Disk, 16) + options.ConfigDiskMiB(request.Engine)
	vm.Options = options
	m.vms = append(m.vms, vm)
	if m.loseCreate {
		api.WriteJSON(w, 502, api.Failure[model.Unit]("injected response loss after create"))
	} else {
		api.WriteJSON(w, 201, api.Success(api.CreateVmResp{Vm: vm}))
	}
}
func (m *mockAgent) deleteHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	before := m.beforeDelete
	m.mu.Unlock()
	if before != nil {
		before()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failDelete {
		api.WriteJSON(w, 500, api.Failure[model.Unit]("injected delete failure"))
		return
	}
	m.vms = slices.DeleteFunc(m.vms, func(vm model.Vm) bool { return vm.ID == r.PathValue("id") })
	api.WriteJSON(w, 200, api.Empty())
}
func (m *mockAgent) actionHTTP(w http.ResponseWriter, r *http.Request, start bool) {
	m.mu.Lock()
	before := m.beforeStart
	m.mu.Unlock()
	if start && before != nil {
		before()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failAction {
		api.WriteJSON(w, 500, api.Failure[model.Unit]("injected action failure"))
		return
	}
	for i := range m.vms {
		if m.vms[i].ID == r.PathValue("id") {
			m.vms[i].State = model.VmStopped
			if start {
				m.vms[i].State = model.VmRunning
			}
			m.vms[i].Error = nil
		}
	}
	api.WriteJSON(w, 200, api.Empty())
}
func (m *mockAgent) resizeHTTP(w http.ResponseWriter, r *http.Request) {
	target, ok := api.ReadJSON[model.VmResources](w, r)
	if !ok {
		return
	}
	m.resizes.Add(1)
	m.mu.Lock()
	before := m.beforeResize
	m.mu.Unlock()
	if before != nil {
		before()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rejectResize {
		api.WriteJSON(w, 409, api.Failure[model.Unit]("injected resize rejection"))
		return
	}
	for i := range m.vms {
		vm := &m.vms[i]
		if vm.ID != r.PathValue("id") {
			continue
		}
		vm.CPU = target.CPU
		vm.Mem = target.Mem
		vm.Disk = target.Disk + vm.Options.ConfigDiskMiB(vm.Engine)
		vm.Options.RequestedDisk = target.Disk
		vm.PendingResources = nil
		vm.Error = nil
		if m.loseResize {
			api.WriteJSON(w, 502, api.Failure[model.Unit]("injected response loss after resize"))
		} else {
			api.WriteJSON(w, 200, api.Success(*vm))
		}
		return
	}
	api.WriteJSON(w, 404, api.Failure[model.Unit]("missing VM"))
}
func controllerFixture(t *testing.T) (*Server, *mockAgent) {
	t.Helper()
	db := testDB(t)
	state := newServer(db, nil)
	state.now = func() uint64 { return 1000 }
	mock := &mockAgent{vms: []model.Vm{}, caps: append([]string{}, capability.LegacyAll...), resource: model.Resource{CPUTotal: 4, MemTotal: 4096, DiskTotal: 1024}}
	peer := httptest.NewServer(mock.handler())
	t.Cleanup(peer.Close)
	t.Cleanup(state.workers.Wait)
	_, status, err := state.registerHost(context.Background(), api.RegisterHostReq{Addr: strings.TrimPrefix(peer.URL, "http://")})
	if err != nil || status != 201 {
		t.Fatal(status, err)
	}
	return state, mock
}
func seedController(t *testing.T, s *Server, m *mockAgent) {
	t.Helper()
	vm := vmRecord("vm", "env", "host")
	vm.CPU = 1
	vm.Mem = 128
	vm.Disk = 16
	vm.Options.RequestedDisk = 16
	env := envRecord("env")
	env.VmIds = model.List[string]{vm.ID}
	env.ExpiresAt = 2000
	s.dbMu.Lock()
	err := s.db.PutEnvironment(context.Background(), &env, []model.Vm{vm}, nil)
	s.dbMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.vms = append(m.vms, vm)
	m.mu.Unlock()
}
func environmentRequest(id string) api.CreateEnvReq {
	return api.CreateEnvReq{ID: id, Owner: "tester", VMs: model.List[api.VmSpec]{{Image: "ubuntu", Engine: model.Qemu, CPU: u32(1), Mem: u32(128), Disk: u32(16)}}}
}
func refreshController(t *testing.T, s *Server) {
	t.Helper()
	client, err := s.clientFactory(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s.RefreshAllHosts(context.Background(), client)
}
func TestCreationPersistsCompletePlanBeforeAgentContactAndReconcilesLostReply(t *testing.T) {
	s, m := controllerFixture(t)
	m.mu.Lock()
	m.loseCreate = true
	m.beforeCreate = func(request api.CreateVmReq) {
		env, err := withDB(s, func(db *DB) (*model.Env, error) { return db.GetEnv(context.Background(), request.EnvID) })
		if err != nil || env == nil || env.State != model.EnvCreating {
			t.Errorf("plan absent: %v %v", env, err)
		}
	}
	m.mu.Unlock()
	detail, err := s.createEnvironment(context.Background(), environmentRequest("new"))
	if err != nil || detail.Env.State != model.EnvCreating || len(detail.VMs) != 1 {
		t.Fatal(detail, err)
	}
	s.workers.Wait()
	actual, err := s.environmentDetail(context.Background(), "new")
	if err != nil || actual.Env.State != model.EnvActive || actual.VMs[0].State != model.VmRunning || m.creates.Load() != 1 {
		t.Fatal(actual, err)
	}
	if _, err := s.createEnvironment(context.Background(), environmentRequest("new")); err == nil || err.Status != 409 {
		t.Fatal(err)
	}
}
func TestPartialDeletePreservesFailedVMAndExpiryRetries(t *testing.T) {
	s, m := controllerFixture(t)
	seedController(t, s, m)
	m.mu.Lock()
	m.failDelete = true
	m.mu.Unlock()
	if err := s.deleteEnvironment(context.Background(), "env", false); err == nil || err.Status != 502 {
		t.Fatal(err)
	}
	detail, err := s.environmentDetail(context.Background(), "env")
	if err != nil || detail.Env.State != model.EnvDeleting || len(detail.VMs) != 1 {
		t.Fatal(detail, err)
	}
	m.mu.Lock()
	m.failDelete = false
	m.mu.Unlock()
	s.ExpireOnce(context.Background())
	if _, err := s.environmentDetail(context.Background(), "env"); err == nil || err.Status != 404 {
		t.Fatal(err)
	}
}
func TestFailedStopDoesNotReportStopped(t *testing.T) {
	s, m := controllerFixture(t)
	seedController(t, s, m)
	m.mu.Lock()
	m.failAction = true
	m.mu.Unlock()
	if err := s.changeEnvironment(context.Background(), "env", "stop"); err == nil || err.Status != 502 {
		t.Fatal(err)
	}
	detail, err := s.environmentDetail(context.Background(), "env")
	if err != nil || detail.Env.State != model.EnvFailed || detail.VMs[0].State != model.VmRunning {
		t.Fatal(detail, err)
	}
}
func TestMissingVMRemainsVisibleForCleanup(t *testing.T) {
	s, m := controllerFixture(t)
	seedController(t, s, m)
	m.mu.Lock()
	m.vms = nil
	m.mu.Unlock()
	refreshController(t, s)
	detail, err := s.environmentDetail(context.Background(), "env")
	if err != nil || len(detail.VMs) != 1 || detail.VMs[0].State != model.VmFailed || detail.VMs[0].Error == nil {
		t.Fatal(detail, err)
	}
	host, dbErr := withDB(s, func(db *DB) (*model.Host, error) { return db.GetHost(context.Background(), "host") })
	if dbErr != nil || host.Resource.DiskUsed < 16 || host.Resource.CPUUsed < 1 {
		t.Fatal(host, dbErr)
	}
}
func TestResizeRecoversLostReplyAndPreservesRecordedTarget(t *testing.T) {
	s, m := controllerFixture(t)
	seedController(t, s, m)
	if err := s.changeEnvironment(context.Background(), "env", "stop"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.loseResize = true
	m.mu.Unlock()
	target := model.VmResources{CPU: 2, Mem: 256, Disk: 32}
	if _, err := s.resizeVM(context.Background(), "vm", target); err == nil || err.Status != 502 {
		t.Fatal(err)
	}
	stored, err := withDB(s, func(db *DB) (*model.Vm, error) { return db.GetVM(context.Background(), "vm") })
	if err != nil || stored.PendingResources != nil || stored.Disk != 32 || stored.Options.RequestedDisk != 32 {
		t.Fatal(stored, err)
	}
	m.mu.Lock()
	m.loseResize = false
	m.mu.Unlock()
	confirmed, apiErr := s.resizeVM(context.Background(), "vm", target)
	if apiErr != nil || confirmed.CPU != 2 || confirmed.Disk != 32 {
		t.Fatal(confirmed, apiErr)
	}
}

func TestResizeRejectsGrowthBeyondTheHostDiskBudgetBeforeForwarding(t *testing.T) {
	s, m := controllerFixture(t)
	seedController(t, s, m)
	ctx := context.Background()
	if err := s.changeEnvironment(ctx, "env", "stop"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.resource.DiskTotal = 32
	m.mu.Unlock()
	if _, err := s.resizeVM(ctx, "vm", model.VmResources{CPU: 1, Mem: 128, Disk: 64}); err == nil || err.Status != 409 {
		t.Fatal("over-budget resize was not rejected", err)
	}
	vm, err := s.db.GetVM(ctx, "vm")
	if err != nil || vm.PendingResources != nil || vm.Disk != 16 || m.resizes.Load() != 0 {
		t.Fatal(vm, err, m.resizes.Load())
	}
}

func TestResizePersistsDiskReservationBeforeForwardingAndReusesItOnRetry(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%v", pending), func(t *testing.T) {
			s, m := controllerFixture(t)
			seedController(t, s, m)
			ctx := context.Background()
			if err := s.changeEnvironment(ctx, "env", "stop"); err != nil {
				t.Fatal(err)
			}
			target := model.VmResources{CPU: 1, Mem: 128, Disk: 32}
			if pending {
				vm, err := s.db.GetVM(ctx, "vm")
				if err != nil {
					t.Fatal(err)
				}
				vm.PendingResources = &target
				if err := s.db.PutVM(ctx, vm); err != nil {
					t.Fatal(err)
				}
			}
			type observation struct {
				disk uint32
				err  error
			}
			observations := make(chan observation, 1)
			m.mu.Lock()
			m.resource.DiskTotal = 32
			m.beforeResize = func() {
				host, err := withDB(s, func(db *DB) (*model.Host, error) { return db.GetHost(ctx, "host") })
				result := observation{err: err}
				if host != nil {
					result.disk = host.Resource.DiskUsed
				}
				observations <- result
			}
			m.mu.Unlock()
			if _, err := s.resizeVM(ctx, "vm", target); err != nil {
				t.Fatal(err)
			}
			observed := <-observations
			if observed.err != nil || observed.disk != 32 {
				t.Fatal("disk reservation was not durable before forwarding", observed)
			}
		})
	}
}

func TestResizeIntentRollsBackWhenHostReservationCannotBeSaved(t *testing.T) {
	s, m := controllerFixture(t)
	seedController(t, s, m)
	ctx := context.Background()
	if err := s.changeEnvironment(ctx, "env", "stop"); err != nil {
		t.Fatal(err)
	}
	execSQL(t, s.db, "CREATE TRIGGER reject_host BEFORE INSERT ON hosts BEGIN SELECT RAISE(ABORT,'injected host failure');END;")
	if _, err := s.resizeVM(ctx, "vm", model.VmResources{CPU: 1, Mem: 128, Disk: 32}); err == nil || err.Status != 500 {
		t.Fatal(err)
	}
	vm, err := s.db.GetVM(ctx, "vm")
	if err != nil || vm.PendingResources != nil || vm.Disk != 16 || m.resizes.Load() != 0 {
		t.Fatal(vm, err, m.resizes.Load())
	}
	host, err := s.db.GetHost(ctx, "host")
	if err != nil || host.Resource.DiskUsed != 16 {
		t.Fatal(host, err)
	}
}
func TestOldSnapshotsCannotOverwriteNewMutationState(t *testing.T) {
	s, m := controllerFixture(t)
	seedController(t, s, m)
	revision := s.revision.Load()
	operation, err := s.beginOperation("env")
	if err != nil {
		t.Fatal(err)
	}
	defer operation.Close()
	if err := s.applySnapshot(context.Background(), "host", hostSnapshot{err: fmt.Errorf("stale transport failure")}, &revision, nil); err != nil {
		t.Fatal(err)
	}
	host, dbErr := withDB(s, func(db *DB) (*model.Host, error) { return db.GetHost(context.Background(), "host") })
	if dbErr != nil || host.State != model.HostOnline {
		t.Fatal(host, dbErr)
	}
}
func TestPendingRestartReservesCapacityAgainstConcurrentCreation(t *testing.T) {
	s, m := controllerFixture(t)
	seedController(t, s, m)
	if err := s.changeEnvironment(context.Background(), "env", "stop"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.resource.CPUTotal = 1
	entered, release := make(chan struct{}), make(chan struct{})
	m.beforeStart = func() { close(entered); <-release }
	m.mu.Unlock()
	var once sync.Once
	defer once.Do(func() { close(release) })
	done := make(chan *APIError, 1)
	go func() { done <- s.changeEnvironment(context.Background(), "env", "start") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("start not entered")
	}
	if _, err := s.createEnvironment(context.Background(), environmentRequest("competing")); err == nil || err.Status != 422 {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start did not finish")
	}
}
func TestStaleExpiryDoesNotDeleteRecreatedPermanentEnvironment(t *testing.T) {
	s, m := controllerFixture(t)
	seedController(t, s, m)
	s.dbMu.Lock()
	env, err := s.db.GetEnv(context.Background(), "env")
	if err == nil {
		env.ExpiresAt = 0
		err = s.db.PutEnv(context.Background(), env)
	}
	s.dbMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.deleteEnvironment(context.Background(), "env", true); err != nil {
		t.Fatal(err)
	}
	detail, apiErr := s.environmentDetail(context.Background(), "env")
	if apiErr != nil || len(detail.VMs) != 1 {
		t.Fatal(detail, apiErr)
	}
}
func TestControllerRoutesServeOnlyExactPublicDashboard(t *testing.T) {
	s, _ := controllerFixture(t)
	key := "fixture-key"
	s.apiKey = &key
	handler := s.Handler()
	for _, tc := range []struct {
		path    string
		status  int
		content string
	}{{"/", 200, "text/html; charset=utf-8"}, {"/api/status", 401, "application/json"}, {"/unknown", 401, "application/json"}} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.status || w.Header().Get("Content-Type") != tc.content {
			t.Fatal(tc.path, w.Code, w.Header())
		}
	}
	w := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/unknown", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	handler.ServeHTTP(w, request)
	if w.Code != 404 || w.Body.Len() != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	request = httptest.NewRequest("DELETE", "/api/envs/absent", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	handler.ServeHTTP(w, request)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 || body["ok"] != true {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	if value, present := body["data"]; !present || value != nil {
		t.Fatal("unit response must include null data", body)
	}
}

func TestStalledDeleteDoesNotBlockAnotherEnvironmentOrHeartbeat(t *testing.T) {
	s, m := controllerFixture(t)
	seedController(t, s, m)
	entered, release := make(chan struct{}), make(chan struct{})
	var gate sync.Once
	m.mu.Lock()
	m.beforeDelete = func() { gate.Do(func() { close(entered) }); <-release }
	m.mu.Unlock()
	var once sync.Once
	defer once.Do(func() { close(release) })
	deleted := make(chan *APIError, 1)
	go func() { deleted <- s.deleteEnvironment(context.Background(), "env", false) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("delete did not enter agent")
	}
	result := make(chan *APIError, 1)
	go func() {
		_, err := s.createEnvironment(context.Background(), environmentRequest("other"))
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("another environment blocked on delete")
	}
	s.workers.Wait()
	refreshController(t, s)
	once.Do(func() { close(release) })
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delete did not finish")
	}
}
func TestEnvironmentCreationSurvivesClientDisconnect(t *testing.T) {
	s, m := controllerFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	m.mu.Lock()
	m.beforeInfo = func() { enterOnce.Do(func() { close(entered) }); <-release }
	m.mu.Unlock()
	host := httptest.NewServer(s.Handler())
	defer host.Close()
	defer releaseOnce.Do(func() { close(release) })
	data, err := json.Marshal(environmentRequest("disconnected"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "POST", host.URL+"/api/envs", strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	done := make(chan error, 1)
	go func() {
		response, err := host.Client().Do(request)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted request did not enter refresh")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("client did not cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("client cancellation blocked")
	}
	releaseOnce.Do(func() { close(release) })
	deadline := time.Now().Add(5 * time.Second)
	for {
		detail, err := s.environmentDetail(context.Background(), "disconnected")
		if err == nil && detail.Env.State == model.EnvActive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("accepted creation was canceled", detail, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func TestLifetimeDefaultsLongTermsAndPermanentEnvironments(t *testing.T) {
	s, _ := controllerFixture(t)
	ctx := context.Background()
	permanent := uint64(0)
	long := uint64(10 * 365 * 24 * 3600)
	for _, tc := range []struct {
		id       string
		lifetime *uint64
		expires  uint64
	}{{"default", nil, 0}, {"permanent", &permanent, 0}, {"long", &long, 1000 + long}} {
		req := environmentRequest(tc.id)
		req.Lifetime = tc.lifetime
		detail, err := s.createEnvironment(ctx, req)
		if err != nil || detail.Env.ExpiresAt != tc.expires {
			t.Fatal(detail, err)
		}
		s.workers.Wait()
	}
	s.ExpireOnce(ctx)
	for _, id := range []string{"default", "permanent", "long"} {
		if _, err := s.environmentDetail(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
}
func TestHostRegistrationRetainsReservationsAndRejectsPeerReplacement(t *testing.T) {
	s, m := controllerFixture(t)
	ctx := context.Background()
	host, err := s.db.GetHost(ctx, "host")
	if err != nil {
		t.Fatal(err)
	}
	host.Resource.CPUUsed = 3
	host.Resource.MemUsed = 512
	host.Resource.DiskUsed = 128
	host.Resource.VmCount = 2
	if err := s.db.PutHost(ctx, host); err != nil {
		t.Fatal(err)
	}
	updated, status, apiErr := s.registerHost(ctx, api.RegisterHostReq{Addr: host.Addr})
	if apiErr != nil || status != 200 || updated.Resource.CPUUsed != 3 || updated.Resource.DiskUsed != 128 || updated.Resource.VmCount != 2 {
		t.Fatal(updated, status, apiErr)
	}
	duplicate := httptest.NewServer(m.handler())
	defer duplicate.Close()
	if _, _, err := s.registerHost(ctx, api.RegisterHostReq{Addr: strings.TrimPrefix(duplicate.URL, "http://")}); err == nil || err.Status != 409 {
		t.Fatal(err)
	}
}

func TestHostMarkerReregistrationRetainsVMsAndReservations(t *testing.T) {
	s, m := controllerFixture(t)
	seedController(t, s, m)
	ctx := context.Background()
	host, err := s.db.GetHost(ctx, "host")
	if err != nil {
		t.Fatal(err)
	}
	address := host.Addr
	vm, err := s.db.GetVM(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	vm.PendingResources = &model.VmResources{CPU: 1, Mem: 128, Disk: 32}
	if err := s.db.PutVM(ctx, vm); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.q.ExecContext(ctx, "UPDATE hosts SET data=?1 WHERE id='host'", "{broken json"); err != nil {
		t.Fatal(err)
	}
	refreshController(t, s)
	// A later failed refresh may replace the marker's diagnostic; recovery
	// must recognize its address/state, not the current error string.
	staleRevision := s.revision.Load()
	stale := hostSnapshot{err: fmt.Errorf("unreachable marker address")}
	if err := s.applySnapshot(ctx, host.ID, stale, &staleRevision, nil); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.vms = nil // An absent agent snapshot must not erase tracked intent.
	m.mu.Unlock()
	updated, status, apiErr := s.registerHost(ctx, api.RegisterHostReq{Addr: address})
	if apiErr != nil || status != 200 {
		t.Fatal(status, apiErr)
	}
	if updated.State != model.HostOnline || updated.Addr != address || updated.Storage != model.File || updated.RegisteredAt != s.now() || updated.Resource.CPUUsed < vm.CPU || updated.Resource.MemUsed < vm.Mem || updated.Resource.DiskUsed < vm.ReservedDisk() || updated.Resource.VmCount != 1 {
		t.Fatal(updated)
	}
	// A refresh begun while the host was a marker cannot put it offline again.
	if err := s.applySnapshot(ctx, host.ID, stale, &staleRevision, nil); err != nil {
		t.Fatal(err)
	}
	host, err = s.db.GetHost(ctx, host.ID)
	if err != nil || host.State != model.HostOnline || host.Addr != address {
		t.Fatal(host, err)
	}
	detail, apiErr := s.environmentDetail(ctx, vm.EnvID)
	if apiErr != nil || len(detail.VMs) != 1 || detail.VMs[0].ID != vm.ID || detail.VMs[0].PendingResources == nil || *detail.VMs[0].PendingResources != *vm.PendingResources {
		t.Fatal(detail, apiErr)
	}
}

func TestFreshResizeRejectionReleasesOnlyItsNewIntent(t *testing.T) {
	s, m := controllerFixture(t)
	seedController(t, s, m)
	ctx := context.Background()
	if err := s.changeEnvironment(ctx, "env", "stop"); err != nil {
		t.Fatal(err)
	}
	target := model.VmResources{CPU: 2, Mem: 256, Disk: 32}
	m.mu.Lock()
	m.rejectResize = true
	m.mu.Unlock()
	if _, err := s.resizeVM(ctx, "vm", target); err == nil || err.Status != 409 {
		t.Fatal(err)
	}
	stored, err := s.db.GetVM(ctx, "vm")
	if err != nil || stored.PendingResources != nil || stored.Disk != 16 {
		t.Fatal(stored, err)
	}
	stored.PendingResources = &target
	message := "earlier outcome unknown"
	stored.Error = &message
	if err := s.db.PutVM(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := s.resizeVM(ctx, "vm", target); err == nil || err.Status != 409 {
		t.Fatal(err)
	}
	retained, err := s.db.GetVM(ctx, "vm")
	if err != nil || retained.PendingResources == nil || *retained.PendingResources != target {
		t.Fatal(retained, err)
	}
}
func TestRecordedResizeCanRetryAfterCapabilityWithdrawal(t *testing.T) {
	s, m := controllerFixture(t)
	seedController(t, s, m)
	ctx := context.Background()
	if err := s.changeEnvironment(ctx, "env", "stop"); err != nil {
		t.Fatal(err)
	}
	target := model.VmResources{CPU: 2, Mem: 256, Disk: 32}
	vm, err := s.db.GetVM(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	vm.PendingResources = &target
	if err := s.db.PutVM(ctx, vm); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.caps = slices.DeleteFunc(m.caps, func(s string) bool { return s == capability.LegacyQemuResources })
	m.mu.Unlock()
	result, apiErr := s.resizeVM(ctx, "vm", target)
	if apiErr != nil || result.PendingResources != nil || result.Disk != 32 {
		t.Fatal(result, apiErr)
	}
	next := target
	next.Disk = 64
	if _, err := s.resizeVM(ctx, "vm", next); err == nil || err.Status != 400 {
		t.Fatal(err)
	}
}
func TestConfirmedStopSurvivesConcurrentlyInvalidatedRefresh(t *testing.T) {
	s, m := controllerFixture(t)
	seedController(t, s, m)
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	var first sync.Once
	m.mu.Lock()
	m.beforeInfo = func() { first.Do(func() { close(entered) }); <-release }
	m.mu.Unlock()
	var released sync.Once
	defer released.Do(func() { close(release) })
	done := make(chan *APIError, 1)
	go func() { done <- s.changeEnvironment(ctx, "env", "stop") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh not entered")
	}
	op, apiErr := s.beginOperation("unrelated")
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	op.Close()
	released.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop blocked")
	}
	vm, err := s.db.GetVM(ctx, "vm")
	if err != nil || vm.State != model.VmStopped {
		t.Fatal(vm, err)
	}
}

func TestCancelledExpiryRetainsDeletionIntentAndReservations(t *testing.T) {
	s, m := controllerFixture(t)
	seedController(t, s, m)
	s.dbMu.Lock()
	env, err := s.db.GetEnv(context.Background(), "env")
	if err == nil {
		env.ExpiresAt = 1
		err = s.db.PutEnv(context.Background(), env)
	}
	s.dbMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	m.mu.Lock()
	m.beforeDelete = func() { close(entered); <-release }
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.workCtx = ctx
	done := make(chan struct{})
	go func() { s.ExpireOnce(ctx); close(done) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("expiry did not reach the agent")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expiry did not cancel its wait")
	}
	detail, apiErr := s.environmentDetail(context.Background(), "env")
	if apiErr != nil || detail.Env.State != model.EnvDeleting || len(detail.VMs) != 1 || detail.VMs[0].Disk == 0 {
		t.Fatal(detail, apiErr)
	}
	once.Do(func() { close(release) })
	m.mu.Lock()
	m.beforeDelete = nil
	m.mu.Unlock()
	s.workCtx = context.Background()
	s.ExpireOnce(context.Background())
	if _, apiErr := s.environmentDetail(context.Background(), "env"); apiErr == nil || apiErr.Status != 404 {
		t.Fatal(apiErr)
	}
}

func TestAdmissionCountsTheManagedSSHSeedTowardTheFileLimit(t *testing.T) {
	s, _ := controllerFixture(t)
	host := httptest.NewServer(s.Handler())
	defer host.Close()
	files := model.GuestConfig{}
	for i := 0; i < 32; i++ {
		files[fmt.Sprintf("file-%02d", i)] = "x"
	}
	spec := api.VmSpec{Image: "ubuntu", Engine: model.Firecracker, CPU: u32(1), Mem: u32(128), Disk: u32(16), GuestConfig: files,
		SSH: &model.SshOptions{User: "user"}, SSHKeys: model.List[string]{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDUMMYDUMMYDUMMYDUMMYDUMMYDUMMYDUMMYDUMMYDU tester"}}
	request := api.CreateEnvReq{ID: "seed-limit", Owner: "tester", VMs: model.List[api.VmSpec]{spec}}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	response, err := host.Client().Post(host.URL+"/api/envs", "application/json", strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 400 || !strings.Contains(string(body), "exceeds 32 files") {
		t.Fatalf("%d: %s", response.StatusCode, body)
	}
	// Without the SSH seed the same 32 caller files stay within the limit.
	spec.SSH, spec.SSHKeys = nil, nil
	request = api.CreateEnvReq{ID: "seed-limit-free", Owner: "tester", VMs: model.List[api.VmSpec]{spec}}
	data, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	response, err = host.Client().Post(host.URL+"/api/envs", "application/json", strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode == 400 && strings.Contains(string(body), "exceeds 32 files") {
		t.Fatalf("seed-inclusive limit applied without SSH: %s", body)
	}
}
