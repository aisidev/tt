package ctl

import (
	"context"
	"encoding/json"
	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/model"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCommandReplayFingerprintAndFences(t *testing.T) {
	db := testDB(t)
	s := newServer(db, nil)
	calls := 0
	handler := s.commandMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; _, _ = w.Write([]byte(`{"ok":true}`)) }))
	call := func(token uint64, key, body string, legacy bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/envs/ws-g1/start", strings.NewReader(body))
		if !legacy {
			raw, _ := json.Marshal(commandEnvelope{Subject: "ws-g1", Generation: 1, Token: token, Key: key, Fingerprint: strings.Repeat("a", 64)})
			r.Header.Set("X-Compute-Command", string(raw))
		}
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, r)
		return result
	}
	if r := call(3, "one", "{}", false); r.Code != 200 {
		t.Fatal(r.Code, r.Body)
	}
	if r := call(3, "one", "{}", false); r.Code != 200 || calls != 1 {
		t.Fatal(r.Code, calls)
	}
	if r := call(3, "one", `{"changed":true}`, false); r.Code != 400 || calls != 1 {
		t.Fatal(r.Code, calls)
	}
	if r := call(4, "two", "{}", false); r.Code != 200 || calls != 2 {
		t.Fatal(r.Code, calls)
	}
	if r := call(3, "one", "{}", false); r.Code != 409 || calls != 2 {
		t.Fatal(r.Code, calls)
	}
	if r := call(0, "", "{}", true); r.Code != 409 || calls != 2 {
		t.Fatal(r.Code, calls)
	}
}
func TestObservationClockSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ctl.db")
	ctx := context.Background()
	db, err := OpenDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := db.nextObservation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = OpenDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	next, err := db.nextObservation(ctx)
	if err != nil || next <= first {
		t.Fatal(first, next, err)
	}
}
func TestPendingCommandRecoveryAndNewerFence(t *testing.T) {
	db := testDB(t)
	s := newServer(db, nil)
	calls := 0
	handler := s.commandMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(202)
	}))
	call := func(token uint64) *httptest.ResponseRecorder {
		r := httptest.NewRequest("DELETE", "/api/envs/ws-g1", nil)
		raw, _ := json.Marshal(commandEnvelope{Subject: "ws-g1", Generation: 1, Token: token, Key: "pending", Fingerprint: strings.Repeat("b", 64)})
		r.Header.Set("X-Compute-Command", string(raw))
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, r)
		return out
	}
	if call(2).Code != 503 {
		t.Fatal("initial outcome")
	}
	if call(2).Code != 202 || calls != 2 {
		t.Fatal("pending intent must replay")
	}
	if call(2).Code != 202 || calls != 2 {
		t.Fatal("success must deduplicate")
	}
}

func TestTypedAgentRefusalPreservesBoundaryClass(t *testing.T) {
	for _, class := range []string{"denied", "terminal", "state"} {
		agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			api.WriteJSON(w, 409, api.Resp[struct{}]{Failure: &api.BoundaryError{Class: class, Code: "refused", Message: "refused"}})
		}))
		client, err := newAgentClient(nil, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		action := "stop"
		err = client.action(context.Background(), model.Host{Addr: strings.TrimPrefix(agent.URL, "http://")}, "vm", &action)
		agent.Close()
		mapped := internal(err)
		if mapped == nil || mapped.Failure == nil || mapped.Failure.Class != class {
			t.Fatal(class, mapped)
		}
	}
}
