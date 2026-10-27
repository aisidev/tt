package agent

import (
	"context"
	"sync"
	"time"

	"github.com/aisidev/tt/internal/engine"
	"github.com/aisidev/tt/model"
)

// observedSnapshot projects fresh process evidence without writing lifecycle,
// journal, networking or disk state. Resource accounting keeps the stored claim;
// stopped evidence alone never undoes an accepted operation's reservation.
func (s *Server) observedSnapshot(parent context.Context, id string) ([]model.Vm, []model.Vm, bool, error) {
	if s.commandJournal.Authority == nil {
		vms, corrupt, err := ReadRecoverySnapshot(parent, s.dbPath, s.runtime.db.stderr)
		return vms, vms, corrupt, err
	}
	select {
	case s.observationLane <- struct{}{}:
		defer func() { <-s.observationLane }()
	default:
		return nil, nil, false, diskError("rate_limited", "observation_busy", "process observation lanes are occupied")
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	control, err := s.commandJournal.ControlReport()
	if err != nil {
		return nil, nil, false, err
	}
	db, err := openDatabase(ctx, s.dbPath, true, 2000)
	if err != nil {
		return nil, nil, false, diskError("unknown", "observation_unavailable", "metadata snapshot unavailable")
	}
	defer db.Close()
	var before uint64
	if err = db.conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&before); err != nil {
		return nil, nil, false, err
	}
	stored, corrupt, err := loadRecoverableVMs(ctx, db.conn, s.runtime.db.stderr)
	if err != nil {
		return nil, nil, false, err
	}
	if len(stored) > model.MaxVMs {
		return nil, nil, corrupt, diskError("terminal", "size_exceeded", "process observation exceeds VM budget")
	}
	if corrupt {
		return nil, nil, true, diskError("unknown", "observation_unavailable", "unreadable ownership prevents a complete process observation")
	}
	selected := []model.Vm{}
	for _, vm := range stored {
		if id == "" || vm.ID == id {
			selected = append(selected, vm)
		}
	}
	observed := append([]model.Vm{}, selected...)
	type result struct {
		index int
		vm    model.Vm
		err   error
	}
	jobs := make(chan int, len(observed))
	results := make(chan result, len(observed))
	for i := range observed {
		jobs <- i
	}
	close(jobs)
	var workers sync.WaitGroup
	for i := 0; i < min(4, len(observed)); i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				vm := observed[index]
				if vm.State == model.VmCreating || vm.State == model.VmDeleting || vm.PendingResources != nil || vm.Backup.Busy() || vm.Options.RootManaged && !vm.Options.CreationComplete {
					results <- result{index: index, vm: vm}
					continue
				}
				implementation, err := s.runtime.engineFactory(vm.Engine, s.runtime.containerRuntime)
				if err == nil {
					observer, ok := implementation.(engine.StateObserver)
					if !ok {
						err = diskError("terminal", "unimplemented", "engine has no read-only process observer")
					} else {
						probe, stop := context.WithTimeout(ctx, 2*time.Second)
						vm.State, err = observer.ObserveState(probe, &vm)
						stop()
					}
				}
				if vm.SSH != nil {
					copy := *vm.SSH
					vm.SSH = &copy
					if vm.State != model.VmRunning {
						vm.SSH.Ready = false
					}
				}
				results <- result{index: index, vm: vm, err: err}
			}
		}()
	}
	workers.Wait()
	close(results)
	unsafe := false
	for result := range results {
		if result.err != nil {
			return nil, nil, false, diskError("unknown", "observation_unavailable", "process state could not be established")
		}
		if selected[result.index].State == model.VmStopped && (result.vm.State == model.VmRunning || result.vm.State == model.VmPaused) {
			unsafe = true
		}
		observed[result.index] = result.vm
	}
	var after uint64
	if err = db.conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&after); err != nil {
		return nil, nil, false, err
	}
	current, err := s.commandJournal.ControlReport()
	if err != nil {
		return nil, nil, false, err
	}
	if before != after || current.Authority != control.Authority || current.Epoch != control.Epoch || current.Phase != control.Phase {
		return nil, nil, false, diskError("state", "observation_changed", "ownership changed during process observation")
	}
	if unsafe {
		s.runtime.observationUnsafe.Store(true)
	}
	return selected, observed, false, nil
}
