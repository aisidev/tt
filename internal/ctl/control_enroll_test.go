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
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

func TestExplicitEnrollmentPinsPlanAndNeverAdoptsAgentWorkloads(t *testing.T) {
	testExplicitEnrollment(t, false, false)
}
func TestOneTimeEnrollmentTokenGuardsMembershipAndOriginalReceiptReplay(t *testing.T) {
	testExplicitEnrollment(t, true, false)
}
func TestEnrollmentEpochChangeDuringColdIdentityReadCannotPublishMembership(t *testing.T) {
	testExplicitEnrollment(t, true, true)
}
func testExplicitEnrollment(t *testing.T, tokenMode, changeEpoch bool) {
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
	var calls atomic.Int32
	if tokenMode {
		parent := t.TempDir()
		if os.Chmod(parent, 0700) != nil {
			t.Fatal("private grant parent")
		}
		store, e := command.CreateEnrollmentStore(parent, uuid.NewString())
		if e != nil {
			t.Fatal(e)
		}
		s.enrollmentStore = &store
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/api/control":
			api.WriteJSON(w, 200, api.Success(model.ControlReport{ActivationVersion: 2, Enabled: true, Authority: state.ID, Epoch: 1, Phase: "mutations", ScopeDigest: strings.Repeat("d", 64), Fence: &model.FencingReceipt{HostID: "host"}, Activations: []model.ActivationReceipt{{HostID: "host", Authority: state.ID, Epoch: 1, ScopeDigest: strings.Repeat("d", 64), Request: model.ActivationRequest{Phase: "mutations", PlanDigest: plan}}}}))
		case "/api/info":
			if changeEpoch {
				changed := state
				changed.Epoch++
				changed.Key = uuid.NewString()
				changed.Phase = "fenced"
				if e := os.WriteFile(path, enrollmentTestJSON(changed), 0600); e != nil {
					t.Error(e)
				}
			}
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
	token := ""
	originalKey := uuid.NewString()
	if tokenMode {
		var e error
		token, e = s.enrollmentStore.Issue(state.ID, 1, uuid.NewString(), request)
		if e != nil {
			t.Fatal(e)
		}
	}
	callWith := func(req model.EnrollmentRequest, key, secret string) *httptest.ResponseRecorder {
		sequence++
		body, _ := json.Marshal(req)
		r := httptest.NewRequest("POST", "/api/control/enroll", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		header, _ := json.Marshal(command.Envelope{Authority: state.ID, Epoch: 1, Subject: "control-" + state.ID + "-g1", Generation: 1, Token: 1<<40 | sequence, Key: key, Fingerprint: fmt.Sprintf("%x", sha256.Sum256(body))})
		r.Header.Set("X-Compute-Command", string(header))
		if secret != "" {
			r.Header.Set("X-Fleet-Enrollment-Token", secret)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	call := func(req model.EnrollmentRequest) *httptest.ResponseRecorder {
		return callWith(req, uuid.NewString(), token)
	}
	if tokenMode {
		for _, secret := range []string{"", "invalid-token"} {
			response := callWith(request, uuid.NewString(), secret)
			if response.Code != 403 {
				t.Fatal("missing token was accepted", response.Code)
			}
		}
		if calls.Load() != 0 {
			t.Fatal("invalid token contacted a receiver")
		}
	}
	wrong := request
	wrong.PlanDigest = strings.Repeat("e", 64)
	if response := call(wrong); response.Code != 409 {
		t.Fatal("enrollment ignored activated plan", response.Code, response.Body.String())
	}
	response := callWith(request, originalKey, token)
	if changeEpoch {
		if response.Code != 409 {
			t.Fatal("epoch change did not fence membership write", response.Code, response.Body.String())
		}
		host, e := s.db.GetHost(context.Background(), "host")
		if e != nil || host != nil {
			t.Fatal("stale remote observation published membership", e)
		}
		return
	}
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	if tokenMode {
		if strings.Contains(response.Body.String(), token) {
			t.Fatal("receipt reflected enrollment secret")
		}
		original := response.Body.String()
		replay := callWith(request, originalKey, "")
		if replay.Code != 200 || replay.Body.String() != original {
			t.Fatal("completed original receipt needed fresh token authority")
		}
		reused := callWith(request, uuid.NewString(), token)
		if reused.Code != 403 {
			t.Fatal("another command reused consumed token", reused.Code)
		}
		// Pending command reconciliation after a lost cache write retains its key.
		if _, e := s.db.conn.ExecContext(context.Background(), "UPDATE command_receipts SET status=0,response=X'' WHERE key=?", originalKey); e != nil {
			t.Fatal(e)
		}
		again := callWith(request, originalKey, token)
		if again.Code != 200 || again.Body.String() != original {
			t.Fatal("original pending command lost its claimed grant", again.Code)
		}
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
	expectedStatus := 409
	if tokenMode {
		expectedStatus = 403
	}
	if response := call(wrong); response.Code != expectedStatus {
		t.Fatal("host identity changed during registration", response.Code, response.Body.String())
	}
}

func enrollmentTestJSON(v any) []byte { b, _ := json.Marshal(v); return b }
