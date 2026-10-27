package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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
		failure(w, diskError("terminal", "missing_identity", "receiver activation requires an external epoch"))
		return
	}
	request, ok := api.ReadJSON[model.ActivationRequest](w, r)
	if !ok {
		return
	}
	authority := s.commandJournal.Authority
	existing, err := authority.CheckActivation(request, envelope.Key, s.runtime.hostID)
	if err != nil {
		failure(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
	defer cancel()
	scope, err := s.runtime.processScope(ctx)
	if err != nil {
		failure(w, diskError("unknown", "scope_unavailable", "receiver scope unavailable"))
		return
	}
	raw, _ := json.Marshal(scope)
	scopeDigest := fmt.Sprintf("%x", sha256.Sum256(raw))
	fence, err := authority.FenceReceipt()
	if err != nil || fence == nil || fence.ScopeDigest != scopeDigest {
		failure(w, diskError("state", "fencing_scope_mismatch", "receiver scope changed after fencing"))
		return
	}
	if existing != nil {
		s.mutation.Lock()
		s.runtime.installQuarantine(*existing)
		err = s.runtime.recount(ctx)
		s.mutation.Unlock()
		if err != nil {
			failure(w, err)
			return
		}
		if err = authority.CommitActivation(*existing); err != nil {
			failure(w, err)
			return
		}
		api.WriteJSON(w, 200, api.Success(*existing))
		return
	}
	for !s.commandJournal.IdleExcept(envelope.Subject) {
		select {
		case <-ctx.Done():
			failure(w, diskError("unknown", "activation_unconfirmed", "earlier receiver commands have not drained"))
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
	s.mutation.Lock()
	defer s.mutation.Unlock()
	if len(s.runtime.transferActive) != 0 {
		failure(w, diskError("unknown", "activation_unconfirmed", "an earlier storage transfer remains active"))
		return
	}
	method := "commands_drained"
	var quarantineMiB uint32
	quarantinePinned := false
	if request.Phase != "gc" {
		select {
		case s.inventoryLane <- struct{}{}:
			defer func() { <-s.inventoryLane }()
		default:
			failure(w, diskError("rate_limited", "inventory_busy", "inventory lane is occupied"))
			return
		}
		snapshot, err := s.physicalInventory(ctx)
		if err != nil {
			failure(w, err)
			return
		}
		if !snapshot.Frozen || !snapshot.Complete || snapshot.Processes != 0 || snapshot.Authority != envelope.Authority || snapshot.Epoch != envelope.Epoch || snapshot.Digest != request.InventoryDigest {
			failure(w, diskError("state", "inventory_changed", "complete frozen inventory does not match reviewed evidence"))
			return
		}
		method = "inventory_stable"
		quarantineMiB, quarantinePinned = snapshot.QuarantineMiB, snapshot.QuarantinePinned
	} else {
		previous, err := authority.Activation("mutations")
		if err != nil || previous == nil {
			failure(w, diskError("state", "activation_unconfirmed", "mutation reservation evidence unavailable"))
			return
		}
		quarantineMiB, quarantinePinned = previous.QuarantineMiB, previous.QuarantinePinned
	}
	state, err := authority.State()
	if err != nil {
		failure(w, err)
		return
	}
	receipt := model.ActivationReceipt{Version: 2, ID: uuid.NewString(), Authority: envelope.Authority, Epoch: envelope.Epoch, DecisionKey: state.Key, HostID: s.runtime.hostID, Key: envelope.Key, Method: method, ScopeDigest: scopeDigest, Request: request, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	receipt.QuarantineMiB, receipt.QuarantinePinned = quarantineMiB, quarantinePinned
	s.runtime.installQuarantine(receipt)
	if err = s.runtime.recount(ctx); err != nil {
		failure(w, err)
		return
	}
	if err = authority.CommitActivation(receipt); err != nil {
		failure(w, err)
		return
	}
	api.WriteJSON(w, 200, api.Success(receipt))
}
