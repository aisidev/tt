// Package backup implements the transport-independent backup operation contract.
package backup

import (
	"fmt"
	"strings"

	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

const MaxRetired = 2

func FromHeaders(key, revision *string, action model.BackupAction, generation *string) (model.BackupRequest, error) {
	var request model.BackupRequest
	if key == nil {
		return request, fmt.Errorf("missing precondition: Idempotency-Key required")
	}
	if revision == nil {
		return request, fmt.Errorf("missing precondition: If-Match required")
	}
	r := *revision
	if len(r) < 2 || r[0] != '"' || r[len(r)-1] != '"' {
		return request, fmt.Errorf("invalid If-Match; use the quoted backup revision")
	}
	request = model.BackupRequest{OperationID: *key, ExpectedRevision: r[1 : len(r)-1], Action: action, Generation: generation}
	return request, Validate(request)
}
func Validate(r model.BackupRequest) error {
	for _, value := range []string{r.OperationID, r.ExpectedRevision} {
		if !ValidToken(value) {
			return fmt.Errorf("invalid backup operation ID or revision")
		}
	}
	if r.Action == model.BackupRestore {
		if r.Generation == nil {
			return fmt.Errorf("restore requires a generation")
		}
		if !ValidToken(*r.Generation) {
			return fmt.Errorf("invalid backup generation")
		}
	} else if r.Generation != nil {
		return fmt.Errorf("generation is only valid for restore")
	}
	return nil
}
func Equal(a, b model.BackupRequest) bool {
	return a.OperationID == b.OperationID && a.ExpectedRevision == b.ExpectedRevision && a.Action == b.Action && ((a.Generation == nil && b.Generation == nil) || (a.Generation != nil && b.Generation != nil && *a.Generation == *b.Generation))
}

// Check returns true for an exact completed replay, before inspecting revision.
// False means new or matching pending work; callers still perform admission.
func Check(s model.BackupState, r model.BackupRequest) (bool, error) {
	if err := Validate(r); err != nil {
		return false, err
	}
	if s.LastResult != nil && s.LastResult.Request.OperationID == r.OperationID {
		if Equal(s.LastResult.Request, r) {
			return true, nil
		}
		return false, fmt.Errorf("conflict: backup operation ID reused with different parameters")
	}
	if s.Pending != nil {
		if Equal(s.Pending.Request, r) {
			return false, nil
		}
		return false, fmt.Errorf("conflict: backup operation unfinished; inspect and retry its exact request")
	}
	if r.ExpectedRevision != s.Revision {
		return false, fmt.Errorf("precondition: stale backup revision")
	}
	return false, nil
}
func ErrorStatus(message string) int {
	switch {
	case strings.HasPrefix(message, "precondition:"):
		return 412
	case strings.HasPrefix(message, "missing precondition:"):
		return 428
	case strings.HasPrefix(message, "not found:"):
		return 404
	case strings.HasPrefix(message, "conflict:"):
		return 409
	case strings.HasPrefix(message, "backup unsupported:"), strings.HasPrefix(message, "invalid "), message == "restore requires a generation", message == "generation is only valid for restore", message == "backup admission disabled":
		return 400
	default:
		return 500
	}
}

// ValidToken matches uuid::Uuid::parse_str, including its case-sensitive URN prefix and
// actual braces. google/uuid.Parse alone accepts arbitrary 38-byte wrappers.
func ValidToken(value string) bool {
	if len(value) == 45 && !strings.HasPrefix(value, "urn:uuid:") {
		return false
	}
	return uuid.Validate(value) == nil
}
