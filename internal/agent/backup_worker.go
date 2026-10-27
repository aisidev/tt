package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/backup"
	"github.com/aisidev/tt/internal/engine"
	"github.com/aisidev/tt/model"
)

func prepareDiskRestore(ctx context.Context, vm *model.Vm) error {
	if runtime.GOOS != "linux" || vm.Engine != model.Firecracker {
		return nil
	}
	eng, err := engine.New(model.Firecracker, nil)
	if err != nil {
		return err
	}
	return eng.(interface {
		PrepareDiskRestore(context.Context, *model.Vm) error
	}).PrepareDiskRestore(ctx, vm)
}
func (s *Server) runBackup(id string, request model.BackupRequest, wait bool) (api.BackupView, error) {
	started := time.Now()
	var setup BackupSetup
	for {
		s.mutation.Lock()
		view, err := s.runtime.BackupView(s.workCtx, id)
		if err != nil {
			s.mutation.Unlock()
			return api.BackupView{}, err
		}
		complete, err := backup.Check(view.Vm.Backup, request)
		if err != nil {
			s.mutation.Unlock()
			return api.BackupView{}, err
		}
		if complete {
			s.mutation.Unlock()
			if view.Vm.Backup.LastResult != nil && view.Vm.Backup.LastResult.Error != nil {
				return api.BackupView{}, fmt.Errorf("%s", *view.Vm.Backup.LastResult.Error)
			}
			return view, nil
		}
		setup, err = s.runtime.BackupClaim(s.workCtx, id, &request)
		s.mutation.Unlock()
		if err == nil {
			break
		}
		var busy backupBusyError
		if wait && errors.As(err, &busy) && time.Since(started) < 300*time.Second {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		return api.BackupView{}, err
	}
	defer func() { s.mutation.Lock(); s.runtime.BackupRelease(id); s.mutation.Unlock() }()
	probe := request.Action == model.BackupCreate && setup.VM.Backup.Pending == nil
	c, err := setup.Context(s.workCtx)
	if err != nil {
		return api.BackupView{}, err
	}
	lock, err := c.Lock()
	if err != nil {
		return api.BackupView{}, err
	}
	defer lock.Close()
	witness, err := c.Inspect(s.workCtx, lock, probe)
	if err != nil {
		return api.BackupView{}, err
	}
	vm, err := mutate(s, func(ctx context.Context, r *Runtime) (*model.Vm, error) {
		return r.BackupAdmit(ctx, id, request, witness)
	})
	if err != nil {
		return api.BackupView{}, err
	}
	if vm.Backup.Pending == nil {
		return api.BackupView{}, fmt.Errorf("backup intent absent")
	}
	pending := *vm.Backup.Pending
	var artifact *model.BackupGeneration
	removed := []string{}
	switch pending.Request.Action {
	case model.BackupCreate:
		generation, createErr := c.Create(s.workCtx, lock, pending)
		err = createErr
		if err == nil {
			artifact = &generation
		}
	case model.BackupRestore:
		if vm.Backup.Current == nil {
			err = fmt.Errorf("not found: backup absent")
			break
		}
		removed, err = c.PrepareRestore(s.workCtx, lock, *vm.Backup.Current, vm.Backup.Retired)
		if err == nil {
			err = s.prepareRestore(s.workCtx, vm)
		}
		if err == nil {
			artifact, err = c.Restore(s.workCtx, lock, pending, *vm.Backup.Current)
		}
	case model.BackupDelete:
		generations := append([]model.BackupGeneration{}, vm.Backup.Retired...)
		if vm.Backup.Current != nil {
			generations = append([]model.BackupGeneration{*vm.Backup.Current}, generations...)
		}
		for _, generation := range generations {
			if err = c.Remove(s.workCtx, lock, generation); err != nil {
				break
			}
		}
	}
	if err != nil {
		removed = nil
	}
	resultErr := err
	view, finishErr := mutate(s, func(ctx context.Context, r *Runtime) (api.BackupView, error) {
		return r.BackupFinish(ctx, id, request, removed, artifact, resultErr)
	})
	if finishErr != nil {
		return api.BackupView{}, finishErr
	}
	if resultErr != nil {
		return api.BackupView{}, resultErr
	}
	return view, nil
}
func (s *Server) cleanupBackup(id string) {
	setup, err := mutate(s, func(ctx context.Context, r *Runtime) (BackupSetup, error) { return r.BackupClaim(ctx, id, nil) })
	if err != nil {
		return
	}
	defer func() { s.mutation.Lock(); s.runtime.BackupRelease(id); s.mutation.Unlock() }()
	removed := []string{}
	var cleanupErr *string
	work := func() error {
		c, err := setup.Context(s.workCtx)
		if err != nil {
			return err
		}
		lock, err := c.Lock()
		if err != nil {
			return err
		}
		defer lock.Close()
		if setup.VM.Backup.Current != nil {
			if _, err := c.Verify(s.workCtx, lock, *setup.VM.Backup.Current); err != nil {
				return err
			}
		}
		failures := []string{}
		for _, old := range setup.VM.Backup.Retired {
			if err := c.Remove(s.workCtx, lock, old); err != nil {
				failures = append(failures, err.Error())
			} else {
				removed = append(removed, old.ID)
			}
		}
		if len(failures) > 0 {
			cleanupErr = stringPointer(strings.Join(failures, "; "))
		}
		return nil
	}
	if err := work(); err != nil {
		removed = nil
		cleanupErr = stringPointer(err.Error())
	}
	_, err = mutate(s, func(ctx context.Context, r *Runtime) (model.Unit, error) {
		return model.Unit{}, r.BackupCleanupFinish(ctx, id, removed, cleanupErr)
	})
	if err != nil {
		fmt.Fprintf(s.runtime.db.stderr, "[agent] backup cleanup %s: %v\n", id, err)
	}
}
