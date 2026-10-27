package ctl

import (
	"io"
	"net/http"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
)

func archiveRoutes(s *Server) []api.Route {
	return []api.Route{
		{Pattern: "/api/volumes/{id}/import/abort", Methods: []string{"POST"}, Handler: s.abortArchiveHTTP},
		{Pattern: "/api/volumes/{id}/archive", Methods: []string{"GET"}, Handler: s.exportArchiveHTTP},
		{Pattern: "/api/volumes/{id}/import", Methods: []string{"POST"}, Handler: s.importArchiveHTTP},
		{Pattern: "/api/images/{id}/fence", Methods: []string{"POST"}, Handler: s.fenceImageHTTP},
		{Pattern: "/api/images/{id}/cache", Methods: []string{"DELETE"}, Handler: s.evictImageHTTP},
		{Pattern: "/api/images/{id}/ensure", Methods: []string{"POST"}, Handler: s.ensureArchiveHTTP},
	}
}
func (s *Server) exportArchiveHTTP(w http.ResponseWriter, r *http.Request) {
	volume, failure := s.getVolume(r.Context(), r.PathValue("id"))
	if failure != nil {
		reply(w, model.Unit{}, failure, 200)
		return
	}
	host, failure := s.diskHost(r.Context(), volume.HostID)
	if failure != nil {
		reply(w, model.Unit{}, failure, 200)
		return
	}
	if !host.StorageFeatures.Archives {
		reply(w, model.Unit{}, diskAPIError("terminal", "unimplemented", "host does not negotiate archive export"), 200)
		return
	}
	client, err := s.clientFactory(30 * time.Minute)
	if err != nil {
		reply(w, model.Unit{}, internal(err), 200)
		return
	}
	response, err := client.request(r.Context(), "GET", host.Addr, "/api/volumes/"+volume.ID+"/archive", nil, nil)
	if err != nil {
		reply(w, model.Unit{}, internal(err), 200)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		data, e := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		if e != nil {
			reply(w, model.Unit{}, internal(e), 200)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(data)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Cache-Control", "no-store")
	if _, err = io.Copy(w, response.Body); err != nil {
		panic(http.ErrAbortHandler)
	}
}
func (s *Server) importArchiveHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := command.CopyContext(s.workCtx, r.Context())
	if failure := requireCommand(ctx); failure != nil {
		reply(w, model.Volume{}, failure, 201)
		return
	}
	body, ok := api.ReadJSON[api.ArchiveImportRequest](w, r)
	if !ok {
		return
	}
	body.Spec.ID = r.PathValue("id")
	body.Pull.ID = body.Spec.ID
	envelope, _ := command.FromContext(ctx)
	if envelope.Subject != body.Spec.ID || envelope.Generation != body.Spec.Generation || body.Spec.SizeMiB == 0 || body.Pull.ExpandedMiB < body.Spec.SizeMiB || len(body.Pull.SHA256) != 64 {
		returnIfDiskFailure(w, "invalid_archive")
		return
	}
	s.dbMu.Lock()
	known, err := s.db.GetVolume(ctx, body.Spec.ID)
	if err == nil && known == nil {
		hosts, e := s.db.ListHosts(ctx)
		if e != nil {
			err = e
		} else {
			var chosen *model.Host
			for i := range hosts {
				h := &hosts[i]
				if h.State == model.HostOnline && h.StorageFeatures.SupportsVolumes() && h.StorageFeatures.Archives && volumeEngineHost(*h, body.Spec, false) && (body.Spec.HostID == "" || body.Spec.HostID == h.ID) && h.Resource.CanFit(0, 0, model.SatAdd(body.Pull.ExpandedMiB, (model.Volume{SizeMiB: body.Spec.SizeMiB, Image: body.Spec.Image}).ReservedMiB())) {
					chosen = h
					break
				}
			}
			if chosen == nil {
				err = &api.BoundaryError{Class: "transient", Code: "capacity_exceeded", Message: "no qualified archive host is available"}
			} else {
				format := "raw"
				if body.Spec.Engine == model.Qemu && chosen.Storage == model.File {
					format = "qcow2"
				}
				known = &model.Volume{ID: body.Spec.ID, Owner: body.Spec.Owner, HostID: chosen.ID, Generation: body.Spec.Generation, SizeMiB: body.Spec.SizeMiB, Engine: body.Spec.Engine, Image: body.Spec.Image, Format: format, Retain: body.Spec.Retain, State: "creating", Phase: "archive", CreatedAt: s.now()}
				chosen.Resource.DiskUsed = model.SatAdd(chosen.Resource.DiskUsed, model.SatAdd(known.ReservedMiB(), body.Pull.ExpandedMiB))
				err = s.db.transaction(ctx, "archive volume admission", func(db *DB) error {
					if e := db.PutVolume(ctx, known); e != nil {
						return e
					}
					return db.PutHost(ctx, chosen)
				})
			}
		}
	}
	s.dbMu.Unlock()
	if err != nil {
		reply(w, model.Volume{}, internal(err), 201)
		return
	}
	if known.Generation != body.Spec.Generation || known.Owner != body.Spec.Owner {
		reply(w, model.Volume{}, diskAPIError("terminal", "fingerprint_conflict", "archive volume has a different owner"), 201)
		return
	}
	host, failure := s.diskHost(ctx, known.HostID)
	if failure != nil {
		reply(w, model.Volume{}, failure, 201)
		return
	}
	body.Spec.HostID = known.HostID
	client, err := s.clientFactory(30 * time.Minute)
	if err != nil {
		reply(w, model.Volume{}, internal(err), 201)
		return
	}
	headers := http.Header{}
	headers.Set("X-Archive-Source", r.Header.Get("X-Archive-Source"))
	response, err := client.request(ctx, "POST", host.Addr, "/api/volumes/"+known.ID+"/import", body, headers)
	if err != nil {
		reply(w, model.Volume{}, internal(err), 201)
		return
	}
	status := response.StatusCode
	result, err := decodeResponse[model.Volume](response)
	if err != nil {
		reply(w, model.Volume{}, internal(err), 201)
		return
	}
	if result.Failure != nil {
		reply(w, model.Volume{}, internal(result.Failure), 201)
		return
	}
	if !successful(status) || !result.OK || result.Data == nil {
		reply(w, model.Volume{}, diskAPIError("unknown", "archive_incomplete", "archive executor did not confirm import"), 201)
		return
	}
	observed := *result.Data
	if observed.ID != known.ID || observed.HostID != known.HostID {
		reply(w, model.Volume{}, diskAPIError("unknown", "identity_mismatch", "archive executor returned another disk"), 201)
		return
	}
	_, err = withDB(s, func(db *DB) (model.Unit, error) { return model.Unit{}, db.PutVolume(ctx, &observed) })
	reply(w, observed, internal(err), 201)
}
func returnIfDiskFailure(w http.ResponseWriter, code string) {
	reply(w, model.Volume{}, diskAPIError("terminal", code, "archive request is invalid"), 201)
}
func (s *Server) ensureArchiveHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := command.CopyContext(s.workCtx, r.Context())
	if failure := requireCommand(ctx); failure != nil {
		reply(w, map[string]any{}, failure, 200)
		return
	}
	body, ok := api.ReadJSON[api.ArchiveImportRequest](w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	body.Pull.ID = id
	if id != "sha256-"+body.Pull.SHA256+"-g1" {
		reply(w, map[string]any{}, diskAPIError("terminal", "invalid_archive", "image must be content addressed"), 200)
		return
	}
	plan, err := s.planImage(ctx, id, body, false)
	if err != nil {
		reply(w, model.Unit{}, internal(err), 200)
		return
	}
	client, err := s.clientFactory(30 * time.Minute)
	if err != nil {
		reply(w, map[string]any{}, internal(err), 200)
		return
	}
	count := 0
	for _, target := range plan.Targets {
		host, failure := s.diskHost(ctx, target.ID)
		if failure != nil {
			reply(w, model.Unit{}, failure, 200)
			return
		}
		if host.Addr != target.Addr {
			reply(w, model.Unit{}, diskAPIError("state", "image_host_changed", "original image receiver is unavailable"), 200)
			return
		}
		headers := http.Header{}
		headers.Set("X-Archive-Source", r.Header.Get("X-Archive-Source"))
		response, e := client.request(ctx, "POST", host.Addr, "/api/images/"+id+"/pull", body, headers)
		if e != nil {
			reply(w, map[string]any{}, internal(e), 200)
			return
		}
		status := response.StatusCode
		result, e := decodeResponse[map[string]any](response)
		if e != nil {
			reply(w, map[string]any{}, internal(e), 200)
			return
		}
		if result.Failure != nil {
			reply(w, map[string]any{}, internal(result.Failure), 200)
			return
		}
		if !successful(status) || !result.OK {
			reply(w, map[string]any{}, diskAPIError("unknown", "archive_incomplete", "image cache remains incomplete"), 200)
			return
		}
		if e = s.confirmImageCatalog(ctx, client, target, id, body.Spec.SizeMiB); e != nil {
			reply(w, model.Unit{}, internal(e), 200)
			return
		}
		count++
	}
	if count == 0 {
		reply(w, map[string]any{}, diskAPIError("transient", "host_unavailable", "no image-pull host is online"), 200)
		return
	}
	reply(w, map[string]any{"id": id, "hosts": count, "state": "ready"}, nil, 200)
}

func (s *Server) abortArchiveHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := command.CopyContext(s.workCtx, r.Context())
	if failure := requireCommand(ctx); failure != nil {
		reply(w, model.Unit{}, failure, 200)
		return
	}
	known, err := withDB(s, func(db *DB) (*model.Volume, error) { return db.GetVolume(ctx, r.PathValue("id")) })
	if err != nil {
		reply(w, model.Unit{}, internal(err), 200)
		return
	}
	if known == nil {
		reply(w, model.Unit{}, diskAPIError("terminal", "not_found", "import was not admitted"), 200)
		return
	}
	host, failure := s.diskHost(ctx, known.HostID)
	if failure != nil {
		reply(w, model.Unit{}, failure, 200)
		return
	}
	if !host.StorageFeatures.ArchiveAbort {
		reply(w, model.Unit{}, diskAPIError("terminal", "unimplemented", "executor does not negotiate unpublished import abort"), 200)
		return
	}
	_, failure = s.mutateVolume(ctx, known.ID, "POST", "/import/abort", nil)
	reply(w, model.Unit{}, failure, 200)
}
