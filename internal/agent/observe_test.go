package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/internal/engine"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

type observedFakeEngine struct {
	engine.Engine
	state model.VmState
	err   error
}

func (e *observedFakeEngine) ObserveState(context.Context, *model.Vm) (model.VmState, error) {
	return e.state, e.err
}
func modernObservationFixture(t *testing.T) (*Server, *fakeEngine, *observedFakeEngine, *model.Vm) {
	t.Helper()
	s, physical, _ := serverFixture(t)
	vm, err := s.runtime.CreateVM(context.Background(), requestFixture())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "authority.json")
	raw, _ := json.Marshal(command.AuthorityState{Version: 1, ID: uuid.NewString(), Epoch: 1, Phase: "fenced", Key: uuid.NewString()})
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.commandJournal.SetAuthority(path); err != nil {
		t.Fatal(err)
	}
	observer := &observedFakeEngine{Engine: physical, state: model.VmStopped}
	s.runtime.engineFactory = func(model.Engine, *engine.ContainerRuntime) (engine.Engine, error) { return observer, nil }
	return s, physical, observer, vm
}
func TestReadOnlyObservationReportsExitWithoutSettlingIntentOrResourceClaims(t *testing.T) {
	s, physical, _, vm := modernObservationFixture(t)
	ctx := context.Background()
	before, err := loadVM(ctx, s.runtime.db.conn, vm.ID)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(before)
	stored, observed, corrupt, err := s.observedSnapshot(ctx, vm.ID)
	if err != nil || corrupt || len(observed) != 1 || observed[0].State != model.VmStopped || stored[0].State != model.VmRunning {
		t.Fatal(stored, observed, corrupt, err)
	}
	after, err := loadVM(ctx, s.runtime.db.conn, vm.ID)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := json.Marshal(after)
	if string(actual) != string(original) || physical.starts != 0 || physical.stops != 0 || physical.destroys != 0 {
		t.Fatal("read observation repaired or replayed durable intent")
	}
	if err = s.runtime.recount(ctx); err != nil {
		t.Fatal(err)
	}
	if s.runtime.resource.CPUUsed == 0 {
		t.Fatal("observed exit released an accepted computation claim")
	}
}
func TestReadOnlyObservationPreservesUncertaintyAndSkipsOldDeletingIntent(t *testing.T) {
	s, physical, observer, vm := modernObservationFixture(t)
	ctx := context.Background()
	observer.err = fmt.Errorf("probe failed")
	if _, _, _, err := s.observedSnapshot(ctx, vm.ID); err == nil {
		t.Fatal("failed observation fabricated a stopped VM")
	}
	vm.State = model.VmDeleting
	if err := saveVM(ctx, s.runtime.db.conn, vm); err != nil {
		t.Fatal(err)
	}
	_, observed, _, err := s.observedSnapshot(ctx, vm.ID)
	if err != nil || observed[0].State != model.VmDeleting || physical.destroys != 0 {
		t.Fatal("read replayed deleting intent", observed, err)
	}
}
