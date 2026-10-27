package model

import (
	"bytes"
	"fmt"
)

// Unit is Rust's (), whose JSON representation is null rather than an object.
type Unit struct{}

func (Unit) MarshalJSON() ([]byte, error) { return []byte("null"), nil }
func (*Unit) UnmarshalJSON(data []byte) error {
	if !bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("expected null")
	}
	return nil
}
