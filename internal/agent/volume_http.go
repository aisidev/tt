package agent

import (
	"context"
	"net/http"
	"strconv"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
)

func diskRoutes(s *Server) []api.Route {
	return []api.Route{
		{Pattern: "/api/volumes", Methods: []string{"GET", "POST"}, Handler: s.volumesHTTP},
		{Pattern: "/api/volumes/{id}", Methods: []string{"GET", "PATCH", "DELETE"}, Handler: s.volumeHTTP},
		{Pattern: "/api/volumes/{id}/attach", Methods: []string{"POST"}, Handler: s.attachVolumeHTTP},
		{Pattern: "/api/volumes/{id}/detach", Methods: []string{"POST"}, Handler: s.detachVolumeHTTP},
		{Pattern: "/api/volumes/{id}/snapshots", Methods: []string{"POST"}, Handler: s.snapshotVolumeHTTP},
		{Pattern: "/api/volume-snapshots", Methods: []string{"GET"}, Handler: s.snapshotsHTTP},
		{Pattern: "/api/volume-snapshots/{id}", Methods: []string{"GET", "DELETE"}, Handler: s.snapshotHTTP},
	}
}
func requireDiskIdentity(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := command.FromContext(r.Context()); ok {
		return true
	}
	failure(w, diskError("terminal", "missing_identity", "disk mutations require a fenced command identity"))
	return false
}
func ReadDiskRecord[T any](ctx context.Context, path, table, id string) (*T, error) {
	db, err := openDatabase(ctx, path, true, 2000)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	version, err := schemaVersion(ctx, db.conn)
	if err != nil {
		return nil, err
	}
	if version != agentSchemaVersion {
		return nil, diskError("terminal", "incompatible_schema", "disk catalog requires a different binary")
	}
	return diskRecord[T](ctx, db.conn, table, id)
}
func readDiskPage[T any](ctx context.Context, path, table, after, owner string, size int) ([]T, string, error) {
	db, err := openDatabase(ctx, path, true, 2000)
	if err != nil {
		return nil, "", err
	}
	defer db.Close()
	rows, err := db.conn.QueryContext(ctx, "SELECT id FROM "+table+" WHERE id>?1 AND (?2='' OR json_extract(data,'$.owner')=?2) AND json_extract(data,'$.state')!='deleted' ORDER BY id LIMIT ?3", after, owner, size+1)
	if err != nil {
		return nil, "", err
	}
	ids := []string{}
	for rows.Next() {
		var id sqlText
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, "", err
		}
		ids = append(ids, string(id))
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(ids) > size {
		ids = ids[:size]
		next = ids[len(ids)-1]
	}
	out := []T{}
	for _, id := range ids {
		v, err := diskRecord[T](ctx, db.conn, table, id)
		if err != nil {
			return nil, "", err
		}
		if v == nil {
			return nil, "", diskError("transient", "unavailable", "disk catalog changed during paging")
		}
		out = append(out, *v)
	}
	return out, next, nil
}
func diskPageSize(r *http.Request) (int, error) {
	size := 100
	if value := r.URL.Query().Get("size"); value != "" {
		var err error
		size, err = strconv.Atoi(value)
		if err != nil || size < 1 || size > 100 {
			return 0, diskError("terminal", "invalid_page", "page size must be within 1..100")
		}
	}
	return size, nil
}
func (s *Server) volumesHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" || r.Method == "HEAD" {
		size, err := diskPageSize(r)
		if err != nil {
			failure(w, err)
			return
		}
		volumes, next, err := readDiskPage[model.Volume](r.Context(), s.dbPath, "volumes", r.URL.Query().Get("after"), r.URL.Query().Get("owner"), size)
		if err != nil {
			failure(w, err)
			return
		}
		api.WriteJSON(w, 200, api.Success(api.VolumePage{Volumes: volumes, NextToken: next, MaxPageSize: 100}))
		return
	}
	if !requireDiskIdentity(w, r) {
		return
	}
	spec, ok := api.ReadJSON[model.VolumeSpec](w, r)
	if !ok {
		return
	}
	volume, err := mutate(s, func(ctx context.Context, runtime *Runtime) (*model.Volume, error) {
		return runtime.CreateVolume(command.CopyContext(ctx, r.Context()), spec)
	})
	if err != nil {
		failure(w, err)
		return
	}
	api.WriteJSON(w, 201, api.Success(*volume))
}
func (s *Server) volumeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "GET" || r.Method == "HEAD" {
		v, err := ReadDiskRecord[model.Volume](r.Context(), s.dbPath, "volumes", id)
		if err == nil && v == nil {
			err = diskError("terminal", "not_found", "volume not found")
		}
		if err != nil {
			failure(w, err)
			return
		}
		api.WriteJSON(w, 200, api.Success(*v))
		return
	}
	if !requireDiskIdentity(w, r) {
		return
	}
	if r.Method == "DELETE" {
		_, err := mutate(s, func(ctx context.Context, runtime *Runtime) (model.Unit, error) {
			return model.Unit{}, runtime.DeleteVolume(command.CopyContext(ctx, r.Context()), id)
		})
		actionReply(w, err)
		return
	}
	update, ok := api.ReadJSON[model.VolumeUpdate](w, r)
	if !ok {
		return
	}
	v, err := mutate(s, func(ctx context.Context, runtime *Runtime) (*model.Volume, error) {
		return runtime.UpdateVolume(command.CopyContext(ctx, r.Context()), id, update)
	})
	if err != nil {
		failure(w, err)
		return
	}
	api.WriteJSON(w, 200, api.Success(*v))
}
func (s *Server) attachVolumeHTTP(w http.ResponseWriter, r *http.Request) {
	if !requireDiskIdentity(w, r) {
		return
	}
	binding, ok := api.ReadJSON[model.VolumeBinding](w, r)
	if !ok {
		return
	}
	_, err := mutate(s, func(ctx context.Context, runtime *Runtime) (model.Unit, error) {
		return model.Unit{}, runtime.AttachVolume(command.CopyContext(ctx, r.Context()), r.PathValue("id"), binding)
	})
	actionReply(w, err)
}
func (s *Server) detachVolumeHTTP(w http.ResponseWriter, r *http.Request) {
	if !requireDiskIdentity(w, r) {
		return
	}
	binding, ok := api.ReadJSON[model.VolumeBinding](w, r)
	if !ok {
		return
	}
	_, err := mutate(s, func(ctx context.Context, runtime *Runtime) (model.Unit, error) {
		return model.Unit{}, runtime.DetachVolume(command.CopyContext(ctx, r.Context()), r.PathValue("id"), binding.VMID)
	})
	actionReply(w, err)
}
func (s *Server) snapshotVolumeHTTP(w http.ResponseWriter, r *http.Request) {
	if !requireDiskIdentity(w, r) {
		return
	}
	request, ok := api.ReadJSON[api.SnapshotVolumeRequest](w, r)
	if !ok {
		return
	}
	point, err := mutate(s, func(ctx context.Context, runtime *Runtime) (*model.VolumeSnapshot, error) {
		return runtime.SnapshotVolume(command.CopyContext(ctx, r.Context()), r.PathValue("id"), request.ID)
	})
	if err != nil {
		failure(w, err)
		return
	}
	api.WriteJSON(w, 201, api.Success(*point))
}
func (s *Server) snapshotsHTTP(w http.ResponseWriter, r *http.Request) {
	size, err := diskPageSize(r)
	if err != nil {
		failure(w, err)
		return
	}
	points, next, err := readDiskPage[model.VolumeSnapshot](r.Context(), s.dbPath, "volume_snapshots", r.URL.Query().Get("after"), r.URL.Query().Get("owner"), size)
	if err != nil {
		failure(w, err)
		return
	}
	api.WriteJSON(w, 200, api.Success(api.VolumeSnapshotPage{Snapshots: points, NextToken: next, MaxPageSize: 100}))
}
func (s *Server) snapshotHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "GET" || r.Method == "HEAD" {
		point, err := ReadDiskRecord[model.VolumeSnapshot](r.Context(), s.dbPath, "volume_snapshots", id)
		if err == nil && point == nil {
			err = diskError("terminal", "not_found", "checkpoint not found")
		}
		if err != nil {
			failure(w, err)
			return
		}
		api.WriteJSON(w, 200, api.Success(*point))
		return
	}
	if !requireDiskIdentity(w, r) {
		return
	}
	_, err := mutate(s, func(ctx context.Context, runtime *Runtime) (model.Unit, error) {
		return model.Unit{}, runtime.DeleteVolumeSnapshot(command.CopyContext(ctx, r.Context()), id)
	})
	actionReply(w, err)
}
