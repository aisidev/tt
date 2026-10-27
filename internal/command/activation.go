package command

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

func phaseRank(phase string) int {
	switch phase {
	case "fenced":
		return 1
	case "inventory":
		return 2
	case "reads":
		return 3
	case "mutations":
		return 4
	case "gc":
		return 5
	}
	return 0
}
func digestValid(s string) bool {
	raw, err := hex.DecodeString(s)
	return err == nil && len(raw) == 32 && strings.ToLower(s) == s
}
func canonicalUUID(s string) bool { id, err := uuid.Parse(s); return err == nil && id.String() == s }
func validActivationReceipt(r model.ActivationReceipt) bool {
	q := r.Request
	if r.Version != 2 || !canonicalUUID(r.ID) || !canonicalUUID(r.Key) || !canonicalUUID(q.FenceID) || !canonicalUUID(q.Previous) || phaseRank(q.Phase) < 3 || r.HostID == "" || !digestValid(q.CatalogDigest) || !digestValid(q.PlanDigest) || !digestValid(r.ScopeDigest) {
		return false
	}
	method := "inventory_stable"
	if q.Phase == "gc" {
		method = "commands_drained"
	}
	if r.HostID == "controller" {
		method = "controller_gate_verified"
		if q.InventoryDigest != "" || r.QuarantineMiB != 0 || r.QuarantinePinned {
			return false
		}
	} else if !digestValid(q.InventoryDigest) {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, r.CompletedAt)
	return err == nil && r.Method == method
}
func readControlDocument(path string, out any) (bool, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, authorityError("unknown", "authority_unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, authorityError("unknown", "authority_unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return false, authorityError("terminal", "malformed_authority")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return false, authorityError("unknown", "authority_unavailable")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > 4096 || decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF {
		return false, authorityError("terminal", "malformed_authority")
	}
	return true, nil
}
func (a *Authority) Activation(phase string) (*model.ActivationReceipt, error) {
	if phaseRank(phase) < 3 {
		return nil, authorityError("terminal", "invalid_activation")
	}
	state, err := a.State()
	if err != nil {
		return nil, err
	}
	var receipt model.ActivationReceipt
	found, err := readControlDocument(a.path+".activation-"+phase, &receipt)
	if err != nil || !found {
		return nil, err
	}
	if receipt.Authority != state.ID || receipt.Epoch != state.Epoch || receipt.DecisionKey != state.Key {
		return nil, nil
	}
	request := receipt.Request
	if request.Phase != phase || !validActivationReceipt(receipt) {
		return nil, authorityError("terminal", "malformed_authority")
	}
	return &receipt, nil
}
func (a *Authority) Active(phase string) bool { return a.RequireActive(phase) == nil }
func (a *Authority) RequireActive(phase string) error {
	state, err := a.State()
	if err != nil {
		return err
	}
	if phaseRank(phase) < 4 || phaseRank(state.Phase) < phaseRank(phase) {
		return authorityError("state", "recovery_fenced")
	}
	fence, err := a.FenceReceipt()
	if err != nil {
		return err
	}
	if fence == nil {
		return authorityError("state", "fence_unconfirmed")
	}
	previous := fence.ID
	var first *model.ActivationReceipt
	for _, name := range []string{"reads", "mutations", "gc"} {
		if phaseRank(name) > phaseRank(state.Phase) {
			break
		}
		receipt, err := a.Activation(name)
		if err != nil {
			return err
		}
		if receipt == nil || receipt.Request.FenceID != fence.ID || receipt.ScopeDigest != fence.ScopeDigest || receipt.HostID != fence.HostID || receipt.Request.Previous != previous {
			return authorityError("state", "activation_unconfirmed")
		}
		if first == nil {
			first = receipt
		} else if receipt.Request.InventoryDigest != first.Request.InventoryDigest || receipt.Request.PlanDigest != first.Request.PlanDigest || receipt.Request.CatalogDigest != first.Request.CatalogDigest || receipt.QuarantineMiB != first.QuarantineMiB || receipt.QuarantinePinned != first.QuarantinePinned {
			return authorityError("state", "activation_chain_mismatch")
		}
		previous = receipt.ID
	}
	return nil
}

// CheckActivation validates a monotone chain. The host handler must separately
// recompute its own complete physical inventory while frozen before committing
// reads or mutations. This journal never accepts an operator boolean proof.
func (a *Authority) CheckActivation(request model.ActivationRequest, key, receiver string) (*model.ActivationReceipt, error) {
	state, err := a.State()
	if err != nil {
		return nil, err
	}
	if phaseRank(request.Phase) < 3 || !canonicalUUID(key) || !canonicalUUID(request.FenceID) || !canonicalUUID(request.Previous) || !digestValid(request.CatalogDigest) || !digestValid(request.PlanDigest) {
		return nil, authorityError("terminal", "invalid_activation")
	}
	fence, err := a.FenceReceipt()
	if err != nil {
		return nil, err
	}
	if fence == nil || fence.ID != request.FenceID || fence.HostID != receiver {
		return nil, authorityError("state", "fence_unconfirmed")
	}
	if receiver != "controller" && !digestValid(request.InventoryDigest) {
		return nil, authorityError("terminal", "invalid_activation")
	}
	if receiver == "controller" && request.InventoryDigest != "" {
		return nil, authorityError("terminal", "invalid_activation")
	}
	existing, err := a.Activation(request.Phase)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.Key != key || existing.HostID != receiver || existing.Request != request {
			return nil, authorityError("terminal", "fingerprint_conflict")
		}
		return existing, nil
	}
	if phaseRank(request.Phase) != phaseRank(state.Phase)+1 {
		return nil, authorityError("state", "recovery_transition")
	}
	if request.Phase == "reads" {
		if request.Previous != fence.ID {
			return nil, authorityError("state", "activation_chain_mismatch")
		}
	} else {
		previous, err := a.Activation(state.Phase)
		if err != nil {
			return nil, err
		}
		if previous == nil || previous.ID != request.Previous || previous.HostID != receiver || previous.ScopeDigest != fence.ScopeDigest || previous.Request.FenceID != request.FenceID || previous.Request.CatalogDigest != request.CatalogDigest || previous.Request.PlanDigest != request.PlanDigest || previous.Request.InventoryDigest != request.InventoryDigest {
			return nil, authorityError("state", "activation_chain_mismatch")
		}
	}
	return nil, nil
}
func (a *Authority) CommitActivation(receipt model.ActivationReceipt) error {
	lock, err := os.OpenFile(a.path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return authorityError("unknown", "authority_unavailable")
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return authorityError("state", "authority_busy")
	}
	state, err := a.State()
	if err != nil {
		return err
	}
	if receipt.Authority != state.ID || receipt.Epoch != state.Epoch || receipt.DecisionKey != state.Key {
		return authorityError("state", "stale_epoch")
	}
	existing, err := a.CheckActivation(receipt.Request, receipt.Key, receipt.HostID)
	if err != nil {
		return err
	}
	if existing != nil {
		receipt = *existing
	} else {
		if !validActivationReceipt(receipt) {
			return authorityError("terminal", "invalid_activation")
		}
		if receipt.Request.Phase != "reads" {
			previous, err := a.Activation(state.Phase)
			if err != nil {
				return err
			}
			if previous == nil || previous.QuarantineMiB != receipt.QuarantineMiB || previous.QuarantinePinned != receipt.QuarantinePinned {
				return authorityError("state", "activation_chain_mismatch")
			}
		}
		fence, err := a.FenceReceipt()
		if err != nil {
			return err
		}
		if fence == nil || fence.ScopeDigest != receipt.ScopeDigest {
			return authorityError("state", "fencing_scope_mismatch")
		}
		if err = a.writeDocument(a.path+".activation-"+receipt.Request.Phase, receipt); err != nil {
			return authorityError("unknown", "authority_unavailable")
		}
	}
	if phaseRank(state.Phase) > phaseRank(receipt.Request.Phase) {
		return nil
	}
	state.Phase = receipt.Request.Phase
	if err = a.writeDocument(a.path, state); err != nil {
		return authorityError("unknown", "authority_unavailable")
	}
	return nil
}
