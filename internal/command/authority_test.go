package command

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

func writeAuthority(t *testing.T, path string, state AuthorityState) {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func activateJournalFixture(t *testing.T, a *Authority) {
	t.Helper()
	state, err := a.State()
	if err != nil {
		t.Fatal(err)
	}
	fence := model.FencingReceipt{Version: 1, ID: uuid.NewString(), Authority: state.ID, Epoch: state.Epoch, HostID: "host", Key: uuid.NewString(), Method: "owned_processes_stopped", ScopeDigest: strings.Repeat("c", 64)}
	if err = a.CommitFence(fence); err != nil {
		t.Fatal(err)
	}
	previous := fence.ID
	for _, phase := range []string{"reads", "mutations"} {
		receipt := model.ActivationReceipt{Version: 2, ID: uuid.NewString(), Authority: state.ID, Epoch: state.Epoch, DecisionKey: state.Key, HostID: "host", Key: uuid.NewString(), Method: "inventory_stable", ScopeDigest: fence.ScopeDigest, Request: model.ActivationRequest{Phase: phase, FenceID: fence.ID, Previous: previous, InventoryDigest: strings.Repeat("a", 64), CatalogDigest: strings.Repeat("b", 64), PlanDigest: strings.Repeat("d", 64)}}
		receipt.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err = a.CommitActivation(receipt); err != nil {
			t.Fatal(err)
		}
		previous = receipt.ID
	}
}
func journalFixture(t *testing.T, path string) (*Journal, func()) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(context.Background(), Schema); err != nil {
		t.Fatal(err)
	}
	return New(conn, &sync.Mutex{}), func() { conn.Close(); db.Close() }
}
func commandRequest(handler http.Handler, envelope Envelope) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(envelope)
	request := httptest.NewRequest("POST", "/"+envelope.Subject, strings.NewReader("{}"))
	request.Header.Set("X-Compute-Command", string(raw))
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, request)
	return result
}
func TestExternalEpochSurvivesSQLiteRollbackAndPrecedesReceiptReplay(t *testing.T) {
	directory := t.TempDir()
	database := filepath.Join(directory, "commands.db")
	path := filepath.Join(directory, "authority.json")
	authority := AuthorityState{Version: 1, ID: uuid.NewString(), Epoch: 1, Phase: "fenced", Key: uuid.NewString()}
	writeAuthority(t, path, authority)
	journal, closeDB := journalFixture(t, database)
	if err := journal.SetAuthority(path); err != nil {
		t.Fatal(err)
	}
	activateJournalFixture(t, journal.Authority)
	calls := 0
	wrap := func(j *Journal) http.Handler {
		return j.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; _, _ = w.Write([]byte(`{"ok":true}`)) }), func(*http.Request) bool { return true }, func(r *http.Request, _ []byte) (string, error) { return strings.TrimPrefix(r.URL.Path, "/"), nil })
	}
	first := Envelope{Authority: authority.ID, Epoch: 1, Subject: "original-g1", Generation: 1, Token: 1<<EpochSequenceBits | 100, Key: "first", Fingerprint: strings.Repeat("a", 64)}
	if result := commandRequest(wrap(journal), first); result.Code != 200 || calls != 1 {
		t.Fatal(result.Code, result.Body.String())
	}
	closeDB()
	snapshot, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	authority.Epoch = 2
	authority.Key = uuid.NewString()
	writeAuthority(t, path, authority)
	// Restoring the old database leaves its old success receipt and watermark.
	if err = os.WriteFile(database, snapshot, 0600); err != nil {
		t.Fatal(err)
	}
	restored, closeRestored := journalFixture(t, database)
	defer closeRestored()
	if err = restored.SetAuthority(path); err != nil {
		t.Fatal(err)
	}
	activateJournalFixture(t, restored.Authority)
	if result := commandRequest(wrap(restored), first); result.Code != 409 || calls != 1 || !strings.Contains(result.Body.String(), "stale_epoch") {
		t.Fatal("old cached success crossed the epoch", result.Code, result.Body.String())
	}
	unseen := first
	unseen.Subject = "unseen-g1"
	if result := commandRequest(wrap(restored), unseen); result.Code != 409 || calls != 1 {
		t.Fatal("unseen subject bypassed global epoch")
	}
	current := first
	current.Epoch = 2
	current.Token = 2<<EpochSequenceBits | 1
	current.Key = "new-epoch"
	if result := commandRequest(wrap(restored), current); result.Code != 200 || calls != 2 {
		t.Fatal("restored lower local sequence could not use a new epoch", result.Code, result.Body.String())
	}
}
func TestRecoveryGateAndUnavailableAuthorityFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authority.json")
	state := AuthorityState{Version: 1, ID: uuid.NewString(), Epoch: 2, Phase: "inventory", Key: uuid.NewString()}
	writeAuthority(t, path, state)
	journal, closeDB := journalFixture(t, ":memory:")
	defer closeDB()
	if err := journal.SetAuthority(path); err != nil {
		t.Fatal(err)
	}
	calls := 0
	handler := journal.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }), func(*http.Request) bool { return true }, func(*http.Request, []byte) (string, error) { return "subject-g1", nil })
	envelope := Envelope{Authority: state.ID, Epoch: 2, Subject: "subject-g1", Generation: 1, Token: 2<<EpochSequenceBits | 1, Key: "effect", Fingerprint: strings.Repeat("b", 64)}
	if result := commandRequest(handler, envelope); result.Code != 409 || calls != 0 {
		t.Fatal("inventory stage admitted a mutation")
	}
	if journal.MutationsAllowed() {
		t.Fatal("background work bypassed recovery gate")
	}
	state.Epoch = 1
	state.Phase = "mutations"
	writeAuthority(t, path, state)
	if result := commandRequest(handler, envelope); result.Code != 409 || calls != 0 {
		t.Fatal("authority rollback admitted a mutation")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if result := commandRequest(handler, envelope); result.Code != 503 || calls != 0 {
		t.Fatal("unavailable authority became permission")
	}
}
