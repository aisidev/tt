package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
)

func TestMutationRechecksFenceAndPreservesIdentityWithoutCallerCancellation(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	server := newServer(r, "")
	envelope := command.Envelope{Subject: "env-g1", Generation: 1, Token: 1, Key: "original", Fingerprint: "original"}
	if _, err := r.db.conn.ExecContext(context.Background(), "INSERT INTO command_fences(subject,token) VALUES(?1,?2)", envelope.Subject, envelope.Token); err != nil {
		t.Fatal(err)
	}
	caller, cancel := context.WithCancel(command.WithContext(context.Background(), envelope))
	cancel()
	_, err := mutate(server, func(work context.Context, _ *Runtime) (model.Unit, error) {
		if work.Err() != nil {
			t.Error("request cancellation cancelled durable work")
		}
		actual, ok := command.FromContext(work)
		if !ok || actual != envelope {
			t.Error("admitted envelope lost")
		}
		return model.Unit{}, nil
	}, caller)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.db.conn.ExecContext(context.Background(), "UPDATE command_fences SET token=2 WHERE subject=?1", envelope.Subject); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = mutate(server, func(context.Context, *Runtime) (model.Unit, error) { called = true; return model.Unit{}, nil }, caller)
	var typed *api.BoundaryError
	if called || !errors.As(err, &typed) || typed.Code != "stale_fence" {
		t.Fatalf("stale waiter dispatched: %v", err)
	}
}
