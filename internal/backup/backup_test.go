package backup

import (
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
	"strings"
	"testing"
)

func TestCompletedRestoreAndStaleReplaysNeverRepeatAWrite(t *testing.T) {
	s := model.NewBackupState()
	generation := uuid.NewString()
	r := model.BackupRequest{OperationID: uuid.NewString(), ExpectedRevision: s.Revision, Action: model.BackupRestore, Generation: &generation}
	s.Revision = uuid.NewString()
	s.LastResult = &model.BackupReceipt{Request: r, CompletedAt: 1}
	replay, err := Check(s, r)
	if err != nil || !replay {
		t.Fatalf("%v %v", replay, err)
	}
	changed := r
	other := uuid.NewString()
	changed.Generation = &other
	if _, err := Check(s, changed); err == nil || !strings.HasPrefix(err.Error(), "conflict:") {
		t.Fatal(err)
	}
	s.LastResult = nil
	if _, err := Check(s, r); err == nil || !strings.HasPrefix(err.Error(), "precondition:") {
		t.Fatal(err)
	}
	s.Pending = &model.BackupPending{Request: r}
	if replay, err := Check(s, r); err != nil || replay {
		t.Fatalf("pending %v %v", replay, err)
	}
}
func TestBackupHeadersAndStatus(t *testing.T) {
	key, revision := uuid.NewString(), uuid.NewString()
	quoted := `"` + revision + `"`
	r, err := FromHeaders(&key, &quoted, model.BackupCreate, nil)
	if err != nil || r.ExpectedRevision != revision {
		t.Fatal(r, err)
	}
	for _, c := range []struct {
		key, revision *string
		status        int
	}{{nil, &quoted, 428}, {&key, nil, 428}, {&key, &revision, 400}} {
		_, err := FromHeaders(c.key, c.revision, model.BackupCreate, nil)
		if err == nil || ErrorStatus(err.Error()) != c.status {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	for message, want := range map[string]int{"precondition: stale": 412, "missing precondition: header": 428, "not found: VM": 404, "conflict: busy": 409, "backup unsupported: file": 400, "invalid generation": 400, "restore requires a generation": 400, "generation is only valid for restore": 400, "backup admission disabled": 400, "storage failed": 500} {
		if got := ErrorStatus(message); got != want {
			t.Errorf("%s: %d", message, got)
		}
	}
}

func TestBackupUUIDFormsMatchRustParser(t *testing.T) {
	token := "67e55044-10b1-426f-9247-bb680e5fe0c8"
	for _, c := range []struct {
		value string
		valid bool
	}{
		{token, true}, {strings.ToUpper(token), true}, {strings.ReplaceAll(token, "-", ""), true}, {"{" + token + "}", true}, {"urn:uuid:" + token, true},
		{"URN:UUID:" + token, false}, {"[" + token + "]", false}, {"x" + token + "x", false}, {" " + token + " ", false},
	} {
		r := model.BackupRequest{OperationID: c.value, ExpectedRevision: token, Action: model.BackupCreate}
		if err := Validate(r); (err == nil) != c.valid {
			t.Errorf("%q: %v", c.value, err)
		}
		r.OperationID = token
		r.Action = model.BackupRestore
		r.Generation = &c.value
		if err := Validate(r); (err == nil) != c.valid {
			t.Errorf("generation %q: %v", c.value, err)
		}
	}
}
