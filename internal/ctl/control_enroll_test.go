package ctl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

func TestExplicitEnrollmentPinsPlanAndNeverAdoptsAgentWorkloads(t *testing.T) {
	s := newServer(testDB(t), nil)
	path := filepath.Join(t.TempDir(), "authority.json")
	state := command.AuthorityState{Version: 1, ID: uuid.NewString(), Epoch: 1, Phase: "fenced", Key: uuid.NewString()}
	raw, _ := json.Marshal(state)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.commandJournal.SetAuthority(path); err != nil {
		t.Fatal(err)
	}
	fence := model.FencingReceipt{Version: 1, ID: uuid.NewString(), Authority: state.ID, Epoch: 1, HostID: "controller", Key: uuid.NewString(), Method: "controller_commands_drained", ScopeDigest: strings.Repeat("c", 64)}
	if err := s.commandJournal.Authority.CommitFence(fence); err != nil {
		t.Fatal(err)
	}
	previous := fence.ID
	plan := strings.Repeat("b", 64)
	for _, phase := range []string{"reads", "mutations"} {
		receipt := model.ActivationReceipt{Version: 2, ID: uuid.NewString(), Authority: state.ID, Epoch: 1, DecisionKey: state.Key, HostID: "controller", Key: uuid.NewString(), Method: "controller_gate_verified", ScopeDigest: fence.ScopeDigest, Request: model.ActivationRequest{Phase: phase, FenceID: fence.ID, Previous: previous, CatalogDigest: strings.Repeat("a", 64), PlanDigest: plan}, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		if err := s.commandJournal.Authority.CommitActivation(receipt); err != nil {
			t.Fatal(err)
		}
		previous = receipt.ID
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/control":
			api.WriteJSON(w, 200, api.Success(model.ControlReport{ActivationVersion: 2, Enabled: true, Authority: state.ID, Epoch: 1, Phase: "mutations", ScopeDigest: strings.Repeat("d", 64), Fence: &model.FencingReceipt{HostID: "host"}, Activations: []model.ActivationReceipt{{HostID: "host", Authority: state.ID, Epoch: 1, ScopeDigest: strings.Repeat("d", 64), Request: model.ActivationRequest{Phase: "mutations", PlanDigest: plan}}}}))
		case "/api/info":
			unowned := vmRecord("unowned-vm", "unowned-g1", "host")
			unowned.State = model.VmStopped
			api.WriteJSON(w, 200, api.Success(api.AgentInfo{HostID: "host", Storage: model.File, Resource: model.Resource{CPUTotal: 2, MemTotal: 512, DiskTotal: 4096}, Engines: []model.Engine{model.Qemu}, Images: []string{}, VMs: &model.List[model.Vm]{unowned}}))
		default:
			api.WriteJSON(w, 404, api.Failure[model.Unit]("unexpected request"))
		}
	}))
	defer peer.Close()
	request := model.EnrollmentRequest{HostID: "host", Endpoint: peer.URL, ScopeDigest: strings.Repeat("d", 64), PlanDigest: plan}
	sequence := uint64(0)
	call := func(req model.EnrollmentRequest) *httptest.ResponseRecorder {
		sequence++
		body, _ := json.Marshal(req)
		r := httptest.NewRequest("POST", "/api/control/enroll", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		header, _ := json.Marshal(command.Envelope{Authority: state.ID, Epoch: 1, Subject: "control-" + state.ID + "-g1", Generation: 1, Token: 1<<40 | sequence, Key: uuid.NewString(), Fingerprint: fmt.Sprintf("%x", sha256.Sum256(body))})
		r.Header.Set("X-Compute-Command", string(header))
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	wrong := request
	wrong.PlanDigest = strings.Repeat("e", 64)
	if response := call(wrong); response.Code != 409 {
		t.Fatal("enrollment ignored activated plan", response.Code, response.Body.String())
	}
	response := call(request)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	host, err := s.db.GetHost(context.Background(), "host")
	if err != nil || host == nil || "http://"+host.Addr != peer.URL {
		t.Fatal(host, err)
	}
	vms, err := s.db.VMsByHost(context.Background(), "host")
	if err != nil || len(vms) != 0 {
		t.Fatal("membership adopted unowned workload records", vms, err)
	}
	wrong = request
	wrong.HostID = "different-host"
	if response := call(wrong); response.Code != 409 {
		t.Fatal("host identity changed during registration", response.Code, response.Body.String())
	}
}
