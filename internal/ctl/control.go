package ctl

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"time"

	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

// The controller can prove command quiescence only. Host process/storage fencing
// is a separate receiver proof and may never be inferred from this receipt.
func (s *Server) fenceControlHTTP(w http.ResponseWriter, r *http.Request) {
	envelope, ok := command.FromContext(r.Context())
	if !ok || s.commandJournal.Authority == nil {
		reply(w, model.Unit{}, diskAPIError("terminal", "missing_identity", "controller fencing requires an external epoch"), 200)
		return
	}
	authority := s.commandJournal.Authority
	state, err := authority.State()
	if err != nil {
		reply(w, model.Unit{}, internal(err), 200)
		return
	}
	if state.ID != envelope.Authority || state.Epoch != envelope.Epoch || (state.Phase != "fenced" && state.Phase != "inventory") {
		reply(w, model.Unit{}, diskAPIError("state", "stale_epoch", "controller epoch changed"), 200)
		return
	}
	if receipt, err := authority.FenceReceipt(); err == nil && receipt != nil && receipt.Key == envelope.Key && receipt.Method == "controller_commands_drained" {
		reply(w, *receipt, nil, 200)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	for {
		state, err = authority.State()
		if err != nil || state.ID != envelope.Authority || state.Epoch != envelope.Epoch {
			reply(w, model.Unit{}, diskAPIError("state", "stale_epoch", "controller epoch changed"), 200)
			return
		}
		s.operationsMu.Lock()
		busy := len(s.operations) > 0
		s.operationsMu.Unlock()
		if !busy && s.commandJournal.IdleExcept(envelope.Subject) {
			break
		}
		select {
		case <-ctx.Done():
			reply(w, model.Unit{}, diskAPIError("unknown", "fence_unconfirmed", "controller commands have not drained"), 200)
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	receipt := model.FencingReceipt{Version: 1, ID: uuid.NewString(), Authority: envelope.Authority, Epoch: envelope.Epoch, HostID: "controller", Key: envelope.Key, Method: "controller_commands_drained", ScopeDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(envelope.Authority+"/controller"))), ProcessDigest: fmt.Sprintf("%x", sha256.Sum256(nil)), Engines: []model.Engine{}, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = authority.CommitFence(receipt); err != nil {
		reply(w, model.Unit{}, internal(err), 200)
		return
	}
	reply(w, receipt, nil, 200)
}
