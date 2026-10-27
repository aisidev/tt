// Package api defines the shared HTTP request and response documents.
package api

import "github.com/aisidev/tt/model"

type Resp[T any] struct {
	OK      bool           `json:"ok"`
	Data    *T             `json:"data,omitempty"`
	Error   *string        `json:"error,omitempty"`
	Failure *BoundaryError `json:"failure,omitempty"`
}

func Success[T any](data T) Resp[T]         { return Resp[T]{OK: true, Data: &data} }
func Failure[T any](message string) Resp[T] { return Resp[T]{Error: &message} }
func Empty() Resp[model.Unit]               { return Resp[model.Unit]{OK: true} }
func (v *Resp[T]) UnmarshalJSON(data []byte) error {
	if err := model.CheckLegacySequence(data, 3); err != nil {
		return err
	}
	type plain Resp[T]
	var next plain
	if err := model.DecodeRecord(data, &next, []string{"ok"}, false, "data", "error"); err != nil {
		return err
	}
	*v = Resp[T](next)
	return nil
}
