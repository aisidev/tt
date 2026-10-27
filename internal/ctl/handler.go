package ctl

import (
	"context"
	_ "embed"
	"net/http"
	"strconv"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/auth"
	"github.com/aisidev/tt/internal/capability"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
)

//go:embed web/index.html
var dashboard []byte

func (s *Server) Handler() http.Handler {
	routes := []api.Route{
		{Pattern: "/api/control/fence", Methods: []string{"POST"}, Handler: s.fenceControlHTTP},
		{Pattern: "/api/control/resume", Methods: []string{"POST"}, Handler: s.resumeControlHTTP},
		{Pattern: "/api/control/enroll", Methods: []string{"POST"}, Handler: s.enrollControlHTTP},
		{Pattern: "/api/control", Methods: []string{"GET"}, Handler: s.controlReportHTTP},
		{Pattern: "/api/storage-capabilities", Methods: []string{"GET"}, Handler: func(w http.ResponseWriter, r *http.Request) {
			api.WriteJSON(w, 200, api.Success(model.StorageFeatures{ProtocolVersion: 1, ArchiveAbort: true, Archives: true, ImagePull: true, ImageCatalog: true, Volumes: true, Snapshots: true, DataDisks: true, WorkspaceSnapshots: true}))
		}},
		{Pattern: "/api/capabilities", Methods: []string{"GET"}, Handler: func(w http.ResponseWriter, r *http.Request) { api.WriteJSON(w, 200, api.Success(capability.Matrix())) }},
		{Pattern: "/api/hosts", Methods: []string{"GET", "POST"}, Handler: s.hostsHTTP},
		{Pattern: "/api/hosts/{id}", Methods: []string{"GET", "DELETE"}, Handler: s.hostHTTP},
		{Pattern: "/api/hosts/{id}/detach", Methods: []string{"POST"}, Handler: s.detachHTTP},
		{Pattern: "/api/envs", Methods: []string{"GET", "POST"}, Handler: s.envsHTTP},
		{Pattern: "/api/envs/{id}", Methods: []string{"GET", "DELETE"}, Handler: s.envHTTP},
		{Pattern: "/api/envs/{id}/stop", Methods: []string{"POST"}, Handler: func(w http.ResponseWriter, r *http.Request) { s.changeHTTP(w, r, "stop") }},
		{Pattern: "/api/envs/{id}/start", Methods: []string{"POST"}, Handler: func(w http.ResponseWriter, r *http.Request) { s.changeHTTP(w, r, "start") }},
		{Pattern: "/api/vms/{id}", Methods: []string{"GET"}, Handler: s.vmHTTP},
		{Pattern: "/api/vms/{id}/resources", Methods: []string{"POST"}, Handler: s.resizeHTTP},
		{Pattern: "/api/vms/{id}/backup", Methods: []string{"GET", "PUT", "DELETE"}, Handler: s.backupHTTP},
		{Pattern: "/api/vms/{id}/backup/restore", Methods: []string{"POST"}, Handler: s.restoreHTTP},
		{Pattern: "/api/images", Methods: []string{"GET"}, Handler: s.imagesHTTP},
		{Pattern: "/api/status", Methods: []string{"GET"}, Handler: s.statusHTTP},
	}
	routes = append(routes, api.Route{Pattern: "/api/envs/{id}/fence", Methods: []string{"POST"}, Handler: s.fenceHTTP}, api.Route{Pattern: "/api/volumes/{id}/fence", Methods: []string{"POST"}, Handler: s.fenceHTTP})
	routes = append(routes, volumeRoutes(s)...)
	routes = append(routes, recoveryRoutes(s)...)
	routes = append(routes, archiveRoutes(s)...)
	protected := api.Router(routes, func(next http.Handler) http.Handler { return auth.Middleware(s.apiKey, s.commandMiddleware(next)) })
	public := api.Router([]api.Route{{Pattern: "/", Methods: []string{"GET"}, Handler: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(dashboard)))
		w.WriteHeader(200)
		_, _ = w.Write(dashboard)
	}}})
	return api.StripHeadBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() == "/" {
			public.ServeHTTP(w, r)
		} else {
			protected.ServeHTTP(w, r)
		}
	}))
}
func reply[T any](w http.ResponseWriter, value T, err *APIError, status int) {
	if err != nil {
		if err.Failure != nil {
			api.WriteJSON(w, err.Status, map[string]any{"ok": false, "failure": err.Failure})
			return
		}
		api.WriteJSON(w, err.Status, api.Failure[T](err.Message))
	} else {
		api.WriteJSON(w, status, api.Success(value))
	}
}
func (s *Server) hostsHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		request, ok := api.ReadJSON[api.RegisterHostReq](w, r)
		if !ok {
			return
		}
		host, status, err := s.registerHost(s.workCtx, request)
		if err != nil {
			reply(w, model.Host{}, err, 0)
		} else {
			reply(w, *host, nil, status)
		}
		return
	}
	hosts, err := withDB(s, func(db *DB) ([]model.Host, error) { return db.ListHosts(r.Context()) })
	reply(w, model.List[model.Host](hosts), internal(err), 200)
}
func (s *Server) hostHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "DELETE" {
		err := s.removeHost(s.workCtx, id)
		if err != nil {
			api.WriteJSON(w, err.Status, api.Failure[model.Unit](err.Message))
		} else {
			api.WriteJSON(w, 200, api.Empty())
		}
		return
	}
	host, err := withDB(s, func(db *DB) (*model.Host, error) { return db.GetHost(r.Context(), id) })
	if err != nil {
		reply(w, model.Host{}, internal(err), 0)
	} else if host == nil {
		reply(w, model.Host{}, apiError(404, "host not found: "+id), 0)
	} else {
		reply(w, *host, nil, 200)
	}
}
func (s *Server) detachHTTP(w http.ResponseWriter, r *http.Request) {
	vms, err := s.detachHost(s.workCtx, r.PathValue("id"))
	reply(w, model.List[model.Vm](vms), err, 200)
}
func (s *Server) envsHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		request, ok := api.ReadJSON[api.CreateEnvReq](w, r)
		if !ok {
			return
		}
		detail, err := s.createEnvironment(command.CopyContext(s.workCtx, r.Context()), request)
		reply(w, detail, err, 202)
		return
	}
	envs, err := withDB(s, func(db *DB) ([]model.Env, error) { return db.ListEnvs(r.Context()) })
	reply(w, model.List[model.Env](envs), internal(err), 200)
}
func (s *Server) envHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "DELETE" {
		reply(w, model.Unit{}, s.deleteEnvironment(command.CopyContext(s.workCtx, r.Context()), r.PathValue("id"), false), 200)
		return
	}
	detail, err := s.environmentDetail(r.Context(), r.PathValue("id"))
	reply(w, detail, err, 200)
}
func (s *Server) changeHTTP(w http.ResponseWriter, r *http.Request, action string) {
	var options model.StopOptions
	if action == "stop" && r.ContentLength != 0 {
		var ok bool
		options, ok = api.ReadJSON[model.StopOptions](w, r)
		if !ok {
			return
		}
	}
	if !model.ValidStopOptions(options) {
		api.WriteJSON(w, 400, api.Resp[model.Unit]{Failure: &api.BoundaryError{Class: "terminal", Code: "invalid_stop_policy", Message: "invalid stop policy"}})
		return
	}
	reply(w, model.Unit{}, s.changeEnvironment(command.CopyContext(s.workCtx, r.Context()), r.PathValue("id"), action, options), 200)
}
func (s *Server) vmHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	vm, err := withDB(s, func(db *DB) (*model.Vm, error) { return db.GetVM(r.Context(), id) })
	if err != nil {
		reply(w, model.Vm{}, internal(err), 0)
	} else if vm == nil {
		reply(w, model.Vm{}, apiError(404, "VM not found: "+id), 0)
	} else {
		reply(w, *vm, nil, 200)
	}
}
func (s *Server) resizeHTTP(w http.ResponseWriter, r *http.Request) {
	target, ok := api.ReadJSON[model.VmResources](w, r)
	if !ok {
		return
	}
	vm, err := s.resizeVM(command.CopyContext(s.workCtx, r.Context()), r.PathValue("id"), target)
	if err != nil {
		reply(w, model.Vm{}, err, 0)
	} else {
		reply(w, *vm, nil, 200)
	}
}
func (s *Server) imagesHTTP(w http.ResponseWriter, r *http.Request) {
	hosts, err := withDB(s, func(db *DB) ([]model.Host, error) { return db.ListHosts(r.Context()) })
	images := model.List[api.ImageInfo]{}
	if err == nil {
		for _, host := range hosts {
			if host.State == model.HostOnline {
				for _, name := range host.Images {
					images = append(images, api.ImageInfo{Name: name, HostID: host.ID})
				}
			}
		}
	}
	reply(w, images, internal(err), 200)
}
func (s *Server) statusHTTP(w http.ResponseWriter, r *http.Request) {
	status, err := withDB(s, func(db *DB) (api.FleetStatus, error) { return db.FleetStatus(r.Context()) })
	reply(w, status, internal(err), 200)
}
func backupReply(w http.ResponseWriter, view api.BackupView, err *APIError) {
	if err != nil {
		api.WriteJSON(w, err.Status, api.Failure[model.Unit](err.Message))
		return
	}
	w.Header().Set("ETag", "\""+view.Vm.Backup.Revision+"\"")
	api.WriteJSON(w, 200, api.Success(view))
}
func (s *Server) backupHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "GET" || r.Method == "HEAD" {
		vm, host, err := s.backupTarget(r.Context(), id)
		if err != nil {
			backupReply(w, api.BackupView{}, err)
			return
		}
		view, err := s.inspectBackup(r.Context(), *vm, *host)
		backupReply(w, view, err)
		return
	}
	action := model.BackupCreate
	if r.Method == "DELETE" {
		action = model.BackupDelete
	}
	view, err := s.backupMutation(s.workCtx, id, r.Header, action, nil)
	backupReply(w, view, err)
}
func (s *Server) restoreHTTP(w http.ResponseWriter, r *http.Request) {
	body, ok := api.ReadJSON[api.RestoreBody](w, r)
	if !ok {
		return
	}
	view, err := s.backupMutation(s.workCtx, r.PathValue("id"), r.Header, model.BackupRestore, &body.Generation)
	backupReply(w, view, err)
}
func (s *Server) ExpireOnce(ctx context.Context) {
	// Modern lifecycle decisions require explicit current-epoch commands. A
	// restored legacy expiry/deletion intent cannot inherit the global gate.
	if s.commandJournal.Authority != nil {
		return
	}
	now := s.now()
	envs, err := withDB(s, func(db *DB) ([]model.Env, error) {
		envs, _, err := db.RecoveryEnvs(ctx)
		return envs, err
	})
	if err != nil {
		s.log("[ctl] expiry DB error: %v", err)
		return
	}
	slots := make(chan struct{}, 8)
	done := make(chan struct{}, len(envs))
	jobs := 0
	for _, env := range envs {
		if env.State != model.EnvDeleting && (env.ExpiresAt == 0 || env.ExpiresAt > now) {
			continue
		}
		jobs++
		go func(id string) {
			slots <- struct{}{}
			defer func() { <-slots; done <- struct{}{} }()
			if err := s.deleteEnvironment(s.workCtx, id, true); err != nil && s.workCtx.Err() == nil {
				s.log("[ctl] cleanup %s: %s", id, err.Message)
			}
		}(env.ID)
	}
	for i := 0; i < jobs; i++ {
		<-done
	}
}
