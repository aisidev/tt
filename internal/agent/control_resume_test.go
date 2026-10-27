//go:build linux

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/internal/storage"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

func TestReceiverActivationRecomputesFrozenInventoryBeforeUnlocking(t *testing.T) {
	s, _, _ := serverFixture(t)
	s.runtime.store = storage.FileStore{}
	s.runtime.runDir = filepath.Join(t.TempDir(), "run")
	for _, path := range []string{s.runtime.runDir, s.runtime.runtimeDir, s.imageDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "authority.json")
	state := command.AuthorityState{Version: 1, ID: uuid.NewString(), Epoch: 1, Phase: "fenced", Key: uuid.NewString()}
	raw, _ := json.Marshal(state)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.commandJournal.SetAuthority(path); err != nil {
		t.Fatal(err)
	}
	scope, err := s.runtime.processScope(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(scope)
	fence := model.FencingReceipt{Version: 1, ID: uuid.NewString(), Authority: state.ID, Epoch: 1, HostID: s.runtime.hostID, Key: uuid.NewString(), Method: "owned_processes_stopped", ScopeDigest: fmt.Sprintf("%x", sha256.Sum256(raw))}
	if err = s.commandJournal.Authority.CommitFence(fence); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.physicalInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	request := model.ActivationRequest{Phase: "reads", FenceID: fence.ID, Previous: fence.ID, InventoryDigest: snapshot.Digest, CatalogDigest: strings.Repeat("a", 64), PlanDigest: strings.Repeat("b", 64)}
	key := uuid.NewString()
	sequence := uint64(0)
	call := func(req model.ActivationRequest) *httptest.ResponseRecorder {
		sequence++
		body, _ := json.Marshal(req)
		httpReq := httptest.NewRequest("POST", "/api/control/resume", bytes.NewReader(body))
		httpReq.Header.Set("Content-Type", "application/json")
		envelope := command.Envelope{Authority: state.ID, Epoch: 1, Subject: "control-" + state.ID + "-g1", Generation: 1, Token: 1<<command.EpochSequenceBits | sequence, Key: key, Fingerprint: fmt.Sprintf("%x", sha256.Sum256(body))}
		header, _ := json.Marshal(envelope)
		httpReq.Header.Set("X-Compute-Command", string(header))
		w := httptest.NewRecorder()
		s.Handler(nil).ServeHTTP(w, httpReq)
		return w
	}
	result := call(request)
	var response api.Resp[model.ActivationReceipt]
	if json.Unmarshal(result.Body.Bytes(), &response) != nil || response.Data == nil || result.Code != 200 {
		t.Fatal(result.Code, result.Body.String())
	}
	if s.commandJournal.MutationsAllowed() {
		t.Fatal("read activation admitted mutations")
	}
	request.Phase = "mutations"
	request.Previous = response.Data.ID
	key = uuid.NewString()
	if err = os.WriteFile(filepath.Join(s.runtime.runtimeDir, "volume-new-orphan-g1"), []byte("held bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	result = call(request)
	if result.Code != 409 || !strings.Contains(result.Body.String(), "inventory_changed") || s.commandJournal.MutationsAllowed() {
		t.Fatal("changed physical evidence opened mutations", result.Code, result.Body.String())
	}
}
