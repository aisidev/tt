package ctl

import (
	"context"
	"net/http"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

func (s *Server) resumeControlHTTP(w http.ResponseWriter, r *http.Request) {
	envelope, ok := command.FromContext(r.Context())
	if !ok || s.commandJournal.Authority == nil {
		reply(w, model.Unit{}, diskAPIError("terminal", "missing_identity", "controller activation requires an external epoch"), 200)
		return
	}
	request, ok := api.ReadJSON[model.ActivationRequest](w, r)
	if !ok {
		return
	}
	authority := s.commandJournal.Authority
	existing, err := authority.CheckActivation(request, envelope.Key, "controller")
	if err != nil {
		reply(w, model.Unit{}, internal(err), 200)
		return
	}
	if existing != nil {
		err = authority.CommitActivation(*existing)
		reply(w, *existing, internal(err), 200)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
	defer cancel()
	for {
		s.operationsMu.Lock()
		busy := len(s.operations) > 0
		s.operationsMu.Unlock()
		if !busy && s.commandJournal.IdleExcept(envelope.Subject) {
			break
		}
		select {
		case <-ctx.Done():
			reply(w, model.Unit{}, diskAPIError("unknown", "activation_unconfirmed", "controller commands have not drained"), 200)
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
	state, err := authority.State()
	if err != nil {
		reply(w, model.Unit{}, internal(err), 200)
		return
	}
	fence, err := authority.FenceReceipt()
	if err != nil || fence == nil {
		reply(w, model.Unit{}, diskAPIError("state", "fence_unconfirmed", "controller fence unavailable"), 200)
		return
	}
	receipt := model.ActivationReceipt{Version: 2, ID: uuid.NewString(), Authority: envelope.Authority, Epoch: envelope.Epoch, DecisionKey: state.Key, HostID: "controller", Key: envelope.Key, Method: "controller_gate_verified", ScopeDigest: fence.ScopeDigest, Request: request, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = authority.CommitActivation(receipt); err != nil {
		reply(w, model.Unit{}, internal(err), 200)
		return
	}
	reply(w, receipt, nil, 200)
}
