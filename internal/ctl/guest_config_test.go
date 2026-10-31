package ctl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/internal/guestconfig"
	"github.com/aisidev/tt/model"
)

func configureSeededStoppedGuest(t *testing.T, s *Server, m *mockAgent) {
	t.Helper()
	seedController(t, s, m)
	if err := s.changeEnvironment(context.Background(), "env", "stop"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	vm := m.vms[0]
	vm.Engine = model.Firecracker
	vm.Options.GuestConfigDigest = guestconfig.Digest(model.GuestConfig{"om-guest.json": "original"})
	m.vms[0] = vm
	m.mu.Unlock()
	s.dbMu.Lock()
	err := s.db.PutVM(context.Background(), &vm)
	s.dbMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}
func TestConfigurationStartRejectsUnnegotiatedHostBeforeLifecycleEffect(t *testing.T) {
	s, m := controllerFixture(t)
	configureSeededStoppedGuest(t, s, m)
	called := false
	m.beforeStart = func() { called = true }
	failure := s.changeEnvironmentConfigured(context.Background(), "env", "start", model.GuestConfig{"om-guest.json": "replacement"})
	if failure == nil || failure.Failure == nil || failure.Failure.Code != "incompatible_protocol" || called {
		t.Fatal("unnegotiated host launched")
	}
	m.mu.Lock()
	state := m.vms[0].State
	m.mu.Unlock()
	if state != model.VmStopped {
		t.Fatal("preflight changed guest state")
	}
}
func TestConfigurationStartForwardsExactPrivateInputAndOriginalCommand(t *testing.T) {
	s, m := controllerFixture(t)
	configureSeededStoppedGuest(t, s, m)
	files := model.GuestConfig{"om-guest.json": "replacement-private-bootstrap"}
	expected := command.Envelope{Subject: "env-g1", Generation: 1, Token: 1, Key: "original", Fingerprint: strings.Repeat("a", 64)}
	base := m.handler()
	starts := 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/guest-config-capabilities":
			api.WriteJSON(w, 200, api.Success(api.GuestConfigCapabilities{ProtocolVersion: 1, StartWithConfig: true}))
		case "/api/vms/vm/start-with-config":
			starts++
			var received api.GuestConfigStart
			var envelope command.Envelope
			if json.NewDecoder(r.Body).Decode(&received) != nil || received.Version != 1 || received.Files["om-guest.json"] != files["om-guest.json"] {
				t.Error("private input changed")
			}
			if json.Unmarshal([]byte(r.Header.Get("X-Compute-Command")), &envelope) != nil || envelope.Key != expected.Key+"/vm" || envelope.Token != expected.Token {
				t.Error("original command lost")
			}
			r.SetPathValue("id", "vm")
			m.actionHTTP(w, r, true)
		default:
			base.ServeHTTP(w, r)
		}
	}))
	defer peer.Close()
	s.dbMu.Lock()
	host, err := s.db.GetHost(context.Background(), "host")
	if err == nil {
		host.Addr = strings.TrimPrefix(peer.URL, "http://")
		err = s.db.PutHost(context.Background(), host)
	}
	s.dbMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.changeEnvironmentConfigured(command.WithContext(context.Background(), expected), "env", "start", files); err != nil {
		t.Fatal(err)
	}
	if starts != 1 {
		t.Fatal("configured start not dispatched exactly once")
	}
}
