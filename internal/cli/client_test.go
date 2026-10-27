package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/model"
)

func TestConfigurationReplacementIsPrivateAtomicAndAddressScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("old\nold-key\n"), 0644); err != nil {
		t.Fatal(err)
	}
	key := "new-key"
	if err := saveConfig(path, "new", &key); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new\nnew-key\n" {
		t.Fatal(string(data), err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	if err := saveConfig(path, "new", nil); err != nil {
		t.Fatal(err)
	}
	saved := loadConfig(path)
	if saved == nil || saved.key == nil || *saved.key != key {
		t.Fatal(saved)
	}
	other := "other"
	address, selected, err := resolveConfig(&other, nil, saved)
	if err != nil || address != other || selected != nil {
		t.Fatal(address, selected, err)
	}
	if err := saveConfig(path, other, nil); err != nil {
		t.Fatal(err)
	}
	saved = loadConfig(path)
	if saved.key != nil {
		t.Fatal("credentials crossed address scope")
	}
	before, _ := os.ReadFile(path)
	bad := "bad\nkey"
	if err := saveConfig(path, other, &bad); err == nil {
		t.Fatal("invalid key saved")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("failed save replaced config")
	}
}
func TestClientRejectsInvalidKeysAndNeverFollowsRedirects(t *testing.T) {
	for _, key := range []string{"", "bad\nkey"} {
		if _, err := newClient("127.0.0.1:9200", &key); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
	var redirected atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		api.WriteJSON(w, 200, api.Success("unexpected"))
	}))
	defer peer.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", peer.URL)
		api.WriteJSON(w, 302, api.Failure[model.Unit]("redirect rejected"))
	}))
	defer source.Close()
	client, err := newClient(source.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetch[string](context.Background(), client, "GET", "/", nil); err == nil || !strings.Contains(err.Error(), "redirect rejected") {
		t.Fatal(err)
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect followed")
	}
}
func TestClientPreservesAuthenticationAndDoesNotRetryMutationFailure(t *testing.T) {
	var calls atomic.Int32
	key := "fixture-key"
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+key || r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing headers")
		}
		api.WriteJSON(w, 500, api.Failure[model.Unit]("unknown outcome"))
	}))
	defer peer.Close()
	client, err := newClient(peer.URL, &key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetch[model.Vm](context.Background(), client, "POST", "/api/vms/vm/resources", model.VmResources{CPU: 1, Mem: 128, Disk: 16}); err == nil || err.Error() != "unknown outcome" {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("mutation retried", calls.Load())
	}
}
