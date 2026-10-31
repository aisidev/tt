package command

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

func enrollmentFixture(t *testing.T) (EnrollmentStore, string, string, model.EnrollmentRequest) {
	t.Helper()
	parent := t.TempDir()
	if os.Chmod(parent, 0700) != nil {
		t.Fatal("private parent")
	}
	store, e := CreateEnrollmentStore(parent, uuid.NewString())
	if e != nil {
		t.Fatal(e)
	}
	return store, uuid.NewString(), uuid.NewString(), model.EnrollmentRequest{HostID: "host", Endpoint: "http://127.0.0.1:9100", ScopeDigest: strings.Repeat("a", 64), PlanDigest: strings.Repeat("b", 64)}
}
func requireEnrollmentCode(t *testing.T, e error, code string) {
	t.Helper()
	var failure *api.BoundaryError
	if !errors.As(e, &failure) || failure.Code != code {
		t.Fatal("wrong typed refusal", e)
	}
}
func TestEnrollmentTokenClaimRetainsOriginalCommandAndNeverChangesPublicRequest(t *testing.T) {
	store, authority, issue, r := enrollmentFixture(t)
	token, e := store.Issue(authority, 1, issue, r)
	if e != nil {
		t.Fatal(e)
	}
	again, e := store.Issue(authority, 1, issue, r)
	if e != nil || again != token {
		t.Fatal("issuance minted another token", e)
	}
	env := Envelope{Authority: authority, Epoch: 1, Key: uuid.NewString()}
	wrong := r
	wrong.HostID = "another"
	requireEnrollmentCode(t, store.Check(token, env, wrong), "enrollment_token_invalid")
	if e = store.Claim(token, env, r); e != nil {
		t.Fatal(e)
	}
	// Reopen without relying on any in-memory token or SQLite state.
	reopened := EnrollmentStore{Directory: store.Directory}
	if e = reopened.Claim(token, env, r); e != nil {
		t.Fatal("original reconciliation lost authority", e)
	}
	another := env
	another.Key = uuid.NewString()
	requireEnrollmentCode(t, reopened.Check(token, another, r), "enrollment_token_used")
	stale := env
	stale.Epoch = 2
	requireEnrollmentCode(t, reopened.Claim(token, stale, r), "enrollment_token_invalid")
	changed := r
	changed.PlanDigest = strings.Repeat("c", 64)
	if _, e = store.Issue(authority, 1, issue, changed); e == nil {
		t.Fatal("original issuance changed scope")
	}
	data, e := os.ReadFile(filepath.Join(store.Directory, "catalog.json"))
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(data), token) {
		t.Fatal("catalog retained plaintext secret")
	}
}
func TestEnrollmentMissingClaimsOrAdmissionRowsCannotBecomeUnusedTokens(t *testing.T) {
	for _, mode := range []string{"claim", "row", "owner", "catalog"} {
		t.Run(mode, func(t *testing.T) {
			store, authority, key, r := enrollmentFixture(t)
			token, e := store.Issue(authority, 1, key, r)
			if e != nil {
				t.Fatal(e)
			}
			env := Envelope{Authority: authority, Epoch: 1, Key: uuid.NewString()}
			if e = store.Claim(token, env, r); e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "claim":
				e = os.Remove(filepath.Join(store.Directory, key+".claim"))
			case "owner":
				e = os.Remove(filepath.Join(store.Directory, "owner.json"))
			case "catalog":
				e = os.Remove(filepath.Join(store.Directory, "catalog.json"))
			case "row":
				var c enrollmentCatalog
				b, err := os.ReadFile(filepath.Join(store.Directory, "catalog.json"))
				if err != nil {
					t.Fatal(err)
				}
				if json.Unmarshal(b, &c) != nil {
					t.Fatal("catalog fixture")
				}
				delete(c.Grants, key)
				e = os.WriteFile(filepath.Join(store.Directory, "catalog.json"), enrollmentJSON(c), 0600)
			}
			if e != nil {
				t.Fatal(e)
			}
			requireEnrollmentCode(t, store.Check(token, env, r), "enrollment_evidence_unavailable")
			if _, e = store.Issue(authority, 1, uuid.NewString(), r); e == nil {
				t.Fatal("lost evidence granted fresh authority")
			}
		})
	}
}
func TestEnrollmentStoreIdentityCannotMoveToAnotherDirectory(t *testing.T) {
	store, _, _, _ := enrollmentFixture(t)
	moved := filepath.Join(filepath.Dir(store.Directory), uuid.NewString())
	if os.Rename(store.Directory, moved) != nil {
		t.Fatal("move fixture")
	}
	if e := (EnrollmentStore{Directory: moved}).Validate(); e == nil {
		t.Fatal("directory spelling manufactured store identity")
	}
	if e := store.Validate(); e == nil {
		t.Fatal("missing original store looked fresh")
	}
}

func TestEnrollmentStoreInitializationReplaysOnlyOriginalIdentity(t *testing.T) {
	parent := t.TempDir()
	if os.Chmod(parent, 0700) != nil {
		t.Fatal("private parent")
	}
	key := uuid.NewString()
	first, e := CreateEnrollmentStore(parent, key)
	if e != nil {
		t.Fatal(e)
	}
	again, e := CreateEnrollmentStore(parent, key)
	if e != nil || again != first {
		t.Fatal("initialization minted another directory identity", e)
	}
	second, e := CreateEnrollmentStore(parent, uuid.NewString())
	if e != nil || second == first {
		t.Fatal("new initialization reused an existing grant scope", e)
	}
	if os.Remove(filepath.Join(first.Directory, "owner.json")) != nil {
		t.Fatal("lost owner fixture")
	}
	if _, e = CreateEnrollmentStore(parent, key); e == nil {
		t.Fatal("missing initialized owner became fresh authority")
	}
}

func TestEnrollmentCompletedReceiptCannotDisappearIntoFreshRegistration(t *testing.T) {
	store, authority, key, r := enrollmentFixture(t)
	token, e := store.Issue(authority, 1, key, r)
	if e != nil {
		t.Fatal(e)
	}
	env := Envelope{Authority: authority, Epoch: 1, Key: uuid.NewString()}
	if e = store.Claim(token, env, r); e != nil {
		t.Fatal(e)
	}
	receipt := model.EnrollmentReceipt{Version: 1, ID: uuid.NewString(), Authority: authority, Epoch: 1, Key: env.Key, Request: r}
	if e = store.Complete(token, env, r, receipt); e != nil {
		t.Fatal(e)
	}
	replay, e := store.Receipt(token, env, r)
	if e != nil || replay == nil || *replay != receipt {
		t.Fatal("original completed result changed", e)
	}
	claim := enrollmentClaim{Version: 1, Key: env.Key, Request: r}
	if os.WriteFile(filepath.Join(store.Directory, key+".claim"), enrollmentJSON(claim), 0600) != nil {
		t.Fatal("lost receipt fixture")
	}
	requireEnrollmentCode(t, store.Check(token, env, r), "enrollment_evidence_unavailable")
}
