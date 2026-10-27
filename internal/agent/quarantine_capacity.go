package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"

	"github.com/aisidev/tt/model"
)

func capacityMiB(bytes uint64) uint32 {
	if bytes > uint64(math.MaxUint32)<<20 {
		return math.MaxUint32
	}
	return uint32((bytes + (1 << 20) - 1) >> 20)
}

// Physical quarantine is an independent admission claim. It is committed with
// external phase evidence so restoring a lower SQLite inventory cannot release
// unknown payload capacity. A fresh reviewed epoch may establish a lower floor.
func (r *Runtime) applyQuarantine(resource *model.Resource) {
	if r.observationUnsafe.Load() {
		pinToCapacity(resource)
		return
	}
	if r.quarantinePinned.Load() {
		resource.DiskUsed = resource.DiskTotal
		return
	}
	resource.DiskUsed = model.SatAdd(resource.DiskUsed, r.quarantineMiB.Load())
}
func (r *Runtime) installQuarantine(receipt model.ActivationReceipt) {
	r.quarantinePinned.Store(true)
	r.quarantineMiB.Store(receipt.QuarantineMiB)
	r.quarantinePinned.Store(receipt.QuarantinePinned)
}
func (s *Server) loadQuarantine(ctx context.Context) error {
	if s.commandJournal.Authority == nil {
		return nil
	}
	s.runtime.quarantinePinned.Store(true)
	a := s.commandJournal.Authority
	state, err := a.State()
	if err != nil {
		return err
	}
	if state.Phase == "mutations" || state.Phase == "gc" {
		if err = a.RequireActive("mutations"); err != nil {
			return err
		}
		scope, err := s.runtime.processScope(ctx)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(scope)
		fence, err := a.FenceReceipt()
		if err != nil {
			return err
		}
		if fence == nil || fence.ScopeDigest != fmt.Sprintf("%x", sha256.Sum256(raw)) {
			return diskError("state", "fencing_scope_mismatch", "serving configuration differs from its physical fencing scope")
		}
		receipt, err := a.Activation(state.Phase)
		if err != nil {
			return err
		}
		if receipt != nil {
			s.runtime.installQuarantine(*receipt)
		}
		if _, _, _, err = s.observedSnapshot(ctx, ""); err != nil {
			return err
		}
	}
	return s.runtime.recount(ctx)
}
