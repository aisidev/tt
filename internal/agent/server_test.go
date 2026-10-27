package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/storage"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

func serverFixture(t *testing.T) (*Server, *fakeEngine, *fakeStore) {
	t.Helper()
	r, e, store := runtimeFixture(t)
	var seq int
	var name, path string
	if err := r.db.conn.QueryRowContext(context.Background(), "PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	return newServer(r, path), e, store
}
func TestAcceptedMutationSurvivesDisconnectAndSnapshotsDoNotWaitForMutationLock(t *testing.T) {
	s, e, _ := serverFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	e.beforeCreate = func(*model.Vm) { close(entered); <-release }
	host := httptest.NewServer(s.Handler(nil))
	defer host.Close()
	defer once.Do(func() { close(release) })
	data, err := json.Marshal(requestFixture())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "POST", host.URL+"/api/vms", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	clientDone := make(chan error, 1)
	go func() {
		response, err := host.Client().Do(request)
		if response != nil {
			response.Body.Close()
		}
		clientDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("mutation not entered")
	}
	cancel()
	select {
	case err := <-clientDone:
		if err == nil {
			t.Fatal("client cancellation not observed")
		}
	case <-time.After(time.Second):
		t.Fatal("client did not cancel")
	}
	bounded, cancelRead := context.WithTimeout(context.Background(), time.Second)
	defer cancelRead()
	for _, path := range []string{"/api/vms", "/api/vms/vm1/backup", "/api/info"} {
		request, err := http.NewRequestWithContext(bounded, "GET", host.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := host.Client().Do(request)
		if err != nil {
			t.Fatal("snapshot blocked", path, err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal(path, response.StatusCode)
		}
	}
	once.Do(func() { close(release) })
	deadline := time.Now().Add(3 * time.Second)
	for {
		vm, err := ReadVM(context.Background(), s.dbPath, "vm1")
		if err != nil {
			t.Fatal(err)
		}
		if vm != nil && vm.State == model.VmRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("disconnected mutation did not finish", vm)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func TestBackupInspectionAndSettledHeaders(t *testing.T) {
	s, _, _ := serverFixture(t)
	ctx := context.Background()
	vm := backupGuest()
	if err := saveVM(ctx, s.runtime.db.conn, &vm); err != nil {
		t.Fatal(err)
	}
	handler := s.Handler(nil)
	request := httptest.NewRequest("PUT", "/api/vms/backup-vm/backup", nil)
	request.Header.Set("Idempotency-Key", uuid.NewString())
	request.Header.Set("If-Match", "\""+vm.Backup.Revision+"\"")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 400 || !strings.Contains(response.Body.String(), "backup admission disabled") || response.Header().Get("X-Tt-Backup-Settled") != request.Header.Get("Idempotency-Key") || response.Header().Get("X-Tt-Backup-Settled-Sequence") != "0" {
		t.Fatal(response.Code, response.Header(), response.Body.String())
	}
	malformed := httptest.NewRequest("PUT", "/api/vms/backup-vm/backup", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, malformed)
	if response.Code != 428 || response.Header().Get("X-Tt-Backup-Settled") != "" {
		t.Fatal(response.Code, response.Header())
	}
	// Inspection reads from SQLite even while another lifecycle holds the mutex.
	s.mutation.Lock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/vms/backup-vm/backup", nil))
		done <- w
	}()
	select {
	case response = <-done:
		s.mutation.Unlock()
	case <-time.After(time.Second):
		s.mutation.Unlock()
		t.Fatal("backup inspection waited for mutation lock")
	}
	if response.Code != 200 || response.Header().Get("ETag") != "\""+vm.Backup.Revision+"\"" {
		t.Fatal(response.Code, response.Header())
	}
}
func TestResizeHTTPStatusMapping(t *testing.T) {
	s, _, _ := serverFixture(t)
	ctx := context.Background()
	if _, err := s.runtime.CreateVM(ctx, requestFixture()); err != nil {
		t.Fatal(err)
	}
	handler := s.Handler(nil)
	send := func(body string) int {
		t.Helper()
		request := httptest.NewRequest("POST", "/api/vms/vm1/resources", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}
	if status := send(`{"cpu":1,"mem":128,"disk":256}`); status != 409 {
		t.Fatal(status)
	}
	if err := s.runtime.StopVM(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}
	if status := send(`{"cpu":0,"mem":128,"disk":256}`); status != 400 {
		t.Fatal(status)
	}
	if status := send(`{"cpu":1,"mem":128,"disk":64}`); status != 400 {
		t.Fatal(status)
	}
	if status := send(`{"cpu":1,"mem":128,"disk":256}`); status != 200 {
		t.Fatal(status)
	}
}

type slowCatalogStore struct {
	*fakeStore
	entered, release chan struct{}
}

func (s *slowCatalogStore) ListImages(context.Context, string) ([]string, error) {
	close(s.entered)
	<-s.release
	return []string{"image"}, nil
}
func TestSlowImageInspectionDoesNotBlockSnapshotsAndPublishesWhenReady(t *testing.T) {
	s, _, store := serverFixture(t)
	slow := &slowCatalogStore{fakeStore: store, entered: make(chan struct{}), release: make(chan struct{})}
	s.catalogStore = func() (storage.ImageStore, error) { return slow, nil }
	s.catalogSlowAfter = 10 * time.Millisecond
	done := make(chan struct{})
	go func() { s.RefreshImages(context.Background()); close(done) }()
	<-slow.entered
	var once sync.Once
	defer once.Do(func() { close(slow.release); <-done })
	deadline := time.Now().Add(time.Second)
	for {
		s.imagesMu.RLock()
		warning := s.images.warning
		isSlow := warning != nil && strings.Contains(*warning, "slow")
		s.imagesMu.RUnlock()
		if isSlow {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("slow warning absent")
		}
		time.Sleep(time.Millisecond)
	}
	response := httptest.NewRecorder()
	s.Handler(nil).ServeHTTP(response, httptest.NewRequest("GET", "/api/images", nil))
	if response.Code != 200 || response.Body.String() != `{"ok":true,"data":[]}` {
		t.Fatal(response.Code, response.Body.String())
	}
	once.Do(func() { close(slow.release); <-done })
	response = httptest.NewRecorder()
	s.Handler(nil).ServeHTTP(response, httptest.NewRequest("GET", "/api/info", nil))
	var result api.Resp[api.AgentInfo]
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Data == nil || len(result.Data.Images) != 1 || result.Data.Images[0] != "image" || result.Data.ImageSizes["image"] != 128 || len(result.Data.Warnings) != 0 {
		t.Fatal(response.Body.String())
	}
}

func TestCorruptInventoryPinsEveryReservationDimensionInInfo(t *testing.T) {
	s, _, _ := serverFixture(t)
	ctx := context.Background()
	if _, err := s.runtime.db.conn.ExecContext(ctx, "INSERT INTO vms VALUES ('corrupt','{broken json')"); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.Handler(nil).ServeHTTP(response, httptest.NewRequest("GET", "/api/info", nil))
	if response.Code != 200 {
		t.Fatal(response.Code)
	}
	var result api.Resp[api.AgentInfo]
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Data == nil {
		t.Fatal(response.Body.String())
	}
	r := result.Data.Resource
	if r.VmCount != model.MaxVMs || r.CPUUsed != r.CPUTotal || r.MemUsed != r.MemTotal || r.DiskUsed != r.DiskTotal {
		t.Fatalf("corrupt inventory must pin every dimension, got %+v", r)
	}
	found := false
	for _, warning := range result.Data.Warnings {
		found = found || strings.Contains(warning, "unreadable VM records")
	}
	if !found {
		t.Fatal(result.Data.Warnings)
	}
}

func TestCommandFenceAndReplayBeforeHostEffect(t *testing.T) {
	s, e, _ := serverFixture(t)
	handler := s.Handler(nil)
	req := requestFixture()
	req.EnvID = "workspace-g1"
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	call := func(token uint64, key string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/vms", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		envelope, _ := json.Marshal(map[string]any{"subject": req.EnvID, "generation": 1, "token": token, "key": key, "fingerprint": strings.Repeat("a", 64)})
		r.Header.Set("X-Compute-Command", string(envelope))
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, r)
		return out
	}
	if out := call(2, "create", data); out.Code != 201 {
		t.Fatal(out.Code, out.Body)
	}
	if out := call(2, "create", data); out.Code != 201 || e.creates != 1 {
		t.Fatal("host repeated accepted effect", out.Code, e.creates)
	}
	if out := call(1, "late", data); out.Code != 409 || e.creates != 1 {
		t.Fatal("host accepted stale fence", out.Code, e.creates)
	}
	legacy := httptest.NewRequest("POST", "/api/vms", bytes.NewReader(data))
	legacy.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, legacy)
	if out.Code != 409 || e.creates != 1 {
		t.Fatal("legacy command bypassed fence", out.Code, e.creates)
	}
}
