package ctl

import (
	"net/http"
	"strconv"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
)

func volumeRoutes(s *Server) []api.Route {
	return []api.Route{
		{Pattern: "/api/volumes", Methods: []string{"GET", "POST"}, Handler: s.volumesHTTP},
		{Pattern: "/api/volumes/{id}", Methods: []string{"GET", "PATCH", "DELETE"}, Handler: s.volumeHTTP},
		{Pattern: "/api/volumes/{id}/attach", Methods: []string{"POST"}, Handler: s.bindVolumeHTTP},
		{Pattern: "/api/volumes/{id}/detach", Methods: []string{"POST"}, Handler: s.bindVolumeHTTP},
		{Pattern: "/api/volumes/{id}/snapshots", Methods: []string{"POST"}, Handler: s.snapshotVolumeHTTP},
		{Pattern: "/api/volume-snapshots", Methods: []string{"GET"}, Handler: s.snapshotsHTTP},
		{Pattern: "/api/volume-snapshots/{id}", Methods: []string{"GET", "DELETE"}, Handler: s.snapshotHTTP},
	}
}
func pageSize(r *http.Request) (int, *APIError) {
	size := 100
	if raw := r.URL.Query().Get("size"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			return 0, diskAPIError("terminal", "invalid_page", "page size must be within 1..100")
		}
		size = n
	}
	return size, nil
}
func (s *Server) volumesHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" || r.Method == "HEAD" {
		size, failure := pageSize(r)
		if failure != nil {
			reply(w, api.VolumePage{}, failure, 200)
			return
		}
		volumes, err := withDB(s, func(db *DB) ([]model.Volume, error) { return db.ListVolumes(r.Context()) })
		if err != nil {
			reply(w, api.VolumePage{}, internal(err), 200)
			return
		}
		out := api.VolumePage{Volumes: []model.Volume{}, MaxPageSize: 100}
		for _, v := range volumes {
			if v.ID <= r.URL.Query().Get("after") || v.State == "deleted" || r.URL.Query().Get("owner") != "" && v.Owner != r.URL.Query().Get("owner") {
				continue
			}
			if len(out.Volumes) == size {
				out.NextToken = out.Volumes[len(out.Volumes)-1].ID
				break
			}
			out.Volumes = append(out.Volumes, v)
		}
		reply(w, out, nil, 200)
		return
	}
	ctx := command.CopyContext(s.workCtx, r.Context())
	if err := requireCommand(ctx); err != nil {
		reply(w, model.Volume{}, err, 201)
		return
	}
	spec, ok := api.ReadJSON[model.VolumeSpec](w, r)
	if !ok {
		return
	}
	volume, err := s.createVolume(ctx, spec)
	reply(w, volume, err, 201)
}
func (s *Server) volumeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "GET" || r.Method == "HEAD" {
		v, err := s.getVolume(r.Context(), id)
		reply(w, v, err, 200)
		return
	}
	ctx := command.CopyContext(s.workCtx, r.Context())
	if err := requireCommand(ctx); err != nil {
		reply(w, model.Volume{}, err, 200)
		return
	}
	var body any
	if r.Method == "PATCH" {
		update, ok := api.ReadJSON[model.VolumeUpdate](w, r)
		if !ok {
			return
		}
		body = update
	}
	v, err := s.mutateVolume(ctx, id, r.Method, "", body)
	reply(w, v, err, 200)
}
func (s *Server) bindVolumeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := command.CopyContext(s.workCtx, r.Context())
	if err := requireCommand(ctx); err != nil {
		reply(w, model.Volume{}, err, 200)
		return
	}
	binding, ok := api.ReadJSON[model.VolumeBinding](w, r)
	if !ok {
		return
	}
	suffix := "/attach"
	if r.URL.Path[len(r.URL.Path)-len("detach"):] == "detach" {
		suffix = "/detach"
	}
	v, err := s.mutateVolume(ctx, r.PathValue("id"), "POST", suffix, binding)
	reply(w, v, err, 200)
}
func (s *Server) snapshotVolumeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := command.CopyContext(s.workCtx, r.Context())
	if err := requireCommand(ctx); err != nil {
		reply(w, model.VolumeSnapshot{}, err, 201)
		return
	}
	request, ok := api.ReadJSON[api.SnapshotVolumeRequest](w, r)
	if !ok {
		return
	}
	point, err := s.snapshotVolume(ctx, r.PathValue("id"), request.ID)
	reply(w, point, err, 201)
}
func (s *Server) snapshotsHTTP(w http.ResponseWriter, r *http.Request) {
	size, failure := pageSize(r)
	if failure != nil {
		reply(w, api.VolumeSnapshotPage{}, failure, 200)
		return
	}
	points, err := withDB(s, func(db *DB) ([]model.VolumeSnapshot, error) { return db.ListVolumeSnapshots(r.Context()) })
	if err != nil {
		reply(w, api.VolumeSnapshotPage{}, internal(err), 200)
		return
	}
	page := api.VolumeSnapshotPage{Snapshots: []model.VolumeSnapshot{}, MaxPageSize: 100}
	for _, p := range points {
		if p.ID <= r.URL.Query().Get("after") || p.State == "deleted" || r.URL.Query().Get("owner") != "" && p.Owner != r.URL.Query().Get("owner") {
			continue
		}
		if len(page.Snapshots) == size {
			page.NextToken = page.Snapshots[len(page.Snapshots)-1].ID
			break
		}
		page.Snapshots = append(page.Snapshots, p)
	}
	reply(w, page, nil, 200)
}
func (s *Server) snapshotHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "GET" || r.Method == "HEAD" {
		point, err := s.getVolumeSnapshot(r.Context(), id)
		reply(w, point, err, 200)
		return
	}
	ctx := command.CopyContext(s.workCtx, r.Context())
	if err := requireCommand(ctx); err != nil {
		reply(w, model.Unit{}, err, 200)
		return
	}
	reply(w, model.Unit{}, s.deleteVolumeSnapshot(ctx, id), 200)
}
