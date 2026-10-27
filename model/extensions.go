package model

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// CheckLegacySequence keeps the pinned positional format closed. Downstream
// extensions are named object fields only; an old extra tuple member must not
// silently acquire a new meaning after a field is added.
func CheckLegacySequence(data []byte, fields int) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil
	}
	var values []json.RawMessage
	if err := json.Unmarshal(trimmed, &values); err != nil {
		return err
	}
	if len(values) > fields {
		return fmt.Errorf("sequence has more than %d legacy fields", fields)
	}
	return nil
}
