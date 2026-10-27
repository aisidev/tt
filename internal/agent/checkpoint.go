package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
)

const checkpointSchema = `CREATE TABLE checkpoint_freezes(vm TEXT PRIMARY KEY,point TEXT NOT NULL,data TEXT NOT NULL);`

func (r *Runtime) freeze(ctx context.Context, id, point string) (model.FrozenVM, error) {
	if model.ValidateName(point, "checkpoint") != nil {
		return model.FrozenVM{}, diskError("terminal", "invalid_snapshot", "invalid checkpoint identity")
	}
	var existingPoint, raw string
	err := r.db.conn.QueryRowContext(ctx, "SELECT point,data FROM checkpoint_freezes WHERE vm=?1", id).Scan(&existingPoint, &raw)
	if err == nil {
		if existingPoint != point {
			return model.FrozenVM{}, diskError("state", "checkpoint_busy", "another checkpoint owns the VM")
		}
		var frozen model.FrozenVM
		if json.Unmarshal([]byte(raw), &frozen) != nil {
			return frozen, diskError("terminal", "malformed", "malformed checkpoint freeze")
		}
		return frozen, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.FrozenVM{}, err
	}
	vm, err := r.requireVM(ctx, id)
	if err != nil {
		return model.FrozenVM{}, err
	}
	if !vm.Options.RootManaged || vm.Options.RootVolumeID == "" {
		return model.FrozenVM{}, diskError("terminal", "unimplemented", "workspace recovery requires a managed root")
	}
	if vm.State != model.VmStopped {
		return model.FrozenVM{}, diskError("state", "workspace_not_stopped", "checkpoint requires a stopped VM")
	}
	engine, err := r.engineFactory(vm.Engine, r.containerRuntime)
	if err != nil {
		return model.FrozenVM{}, err
	}
	actual, err := engine.State(ctx, vm)
	if err != nil {
		return model.FrozenVM{}, err
	}
	if actual != model.VmStopped {
		return model.FrozenVM{}, diskError("state", "workspace_not_stopped", "engine has not confirmed stop")
	}
	ids := []string{vm.Options.RootVolumeID}
	for _, v := range vm.Options.DataVolumes {
		ids = append(ids, v.ID)
	}
	frozen := model.FrozenVM{PointID: point, VM: *vm, Volumes: []model.Volume{}}
	for _, id := range ids {
		v, err := r.volume(ctx, id)
		if err != nil {
			return frozen, err
		}
		if v.AttachedTo != vm.ID || v.State != "attached" || v.PendingSnapshot != "" || v.PendingGroup != "" {
			return frozen, diskError("state", "volume_busy", "disk is not settled for checkpoint")
		}
		if err = r.honorVolumeFence(ctx, id); err != nil {
			return frozen, err
		}
		frozen.Volumes = append(frozen.Volumes, *v)
	}
	tx, err := r.db.conn.BeginTx(ctx, nil)
	if err != nil {
		return frozen, err
	}
	defer tx.Rollback()
	for i := range frozen.Volumes {
		v := frozen.Volumes[i]
		v.PendingGroup = point
		v.Revision++
		if err = saveDisk(ctx, tx, "volumes", v.ID, v); err != nil {
			return frozen, err
		}
	}
	data, err := json.Marshal(frozen)
	if err != nil {
		return frozen, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO checkpoint_freezes(vm,point,data) VALUES(?1,?2,?3)", id, point, string(data)); err != nil {
		return frozen, err
	}
	return frozen, tx.Commit()
}
func (r *Runtime) thaw(ctx context.Context, id, point string) error {
	var current string
	err := r.db.conn.QueryRowContext(ctx, "SELECT point FROM checkpoint_freezes WHERE vm=?1", id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if current != point {
		return diskError("state", "checkpoint_busy", "another checkpoint owns the VM")
	}
	volumes, err := diskList[model.Volume](ctx, r.db.conn, "volumes")
	if err != nil {
		return err
	}
	tx, err := r.db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, v := range volumes {
		if v.PendingGroup != point {
			continue
		}
		if v.PendingSnapshot != "" {
			return diskError("state", "checkpoint_busy", "checkpoint writer has not settled")
		}
		v.PendingGroup = ""
		v.Revision++
		if err = saveDisk(ctx, tx, "volumes", v.ID, v); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM checkpoint_freezes WHERE vm=?1 AND point=?2", id, point); err != nil {
		return err
	}
	return tx.Commit()
}
func (r *Runtime) checkpointWriter(ctx context.Context, v *model.Volume) error {
	if v.PendingGroup == "" {
		return nil
	}
	envelope, ok := command.FromContext(ctx)
	if !ok || envelope.Group != v.PendingGroup {
		return diskError("state", "checkpoint_busy", "workspace checkpoint fences this disk")
	}
	return nil
}
func (s *Server) checkpointHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Method == "GET" || request.Method == "HEAD" {
		frozen, err := mutate(s, func(ctx context.Context, r *Runtime) (model.FrozenVM, error) {
			return r.frozenInfo(ctx, request.PathValue("id"))
		})
		if err != nil {
			failure(w, err)
			return
		}
		api.WriteJSON(w, 200, api.Success(frozen))
		return
	}

	if !requireDiskIdentity(w, request) {
		return
	}
	body, ok := api.ReadJSON[api.CheckpointRequest](w, request)
	if !ok {
		return
	}
	ctx := command.CopyContext(s.workCtx, request.Context())
	if request.URL.Path[len(request.URL.Path)-len("freeze"):] == "freeze" {
		frozen, err := mutate(s, func(_ context.Context, r *Runtime) (model.FrozenVM, error) {
			return r.freeze(ctx, request.PathValue("id"), body.ID)
		})
		if err != nil {
			failure(w, err)
			return
		}
		api.WriteJSON(w, 200, api.Success(frozen))
		return
	}
	_, err := mutate(s, func(_ context.Context, r *Runtime) (model.Unit, error) {
		return model.Unit{}, r.thaw(ctx, request.PathValue("id"), body.ID)
	})
	actionReply(w, err)
}

func (r *Runtime) checkpointGuard(ctx context.Context, id string) error {
	var frozen bool
	if err := r.db.conn.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM checkpoint_freezes WHERE vm=?1)", id).Scan(&frozen); err != nil {
		return err
	}
	if frozen {
		return diskError("state", "checkpoint_busy", "workspace checkpoint owns the stopped VM")
	}
	return nil
}

func (r *Runtime) frozenInfo(ctx context.Context, id string) (model.FrozenVM, error) {
	var raw string
	err := r.db.conn.QueryRowContext(ctx, "SELECT data FROM checkpoint_freezes WHERE vm=?1", id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return model.FrozenVM{}, diskError("terminal", "not_found", "checkpoint freeze not found")
	}
	if err != nil {
		return model.FrozenVM{}, err
	}
	var frozen model.FrozenVM
	if json.Unmarshal([]byte(raw), &frozen) != nil {
		return frozen, diskError("terminal", "malformed", "malformed checkpoint freeze")
	}
	return frozen, nil
}
