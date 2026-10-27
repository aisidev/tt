package ctl

import (
	"net/http"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
)

func recoveryRoutes(s *Server) []api.Route {
	return []api.Route{
		{Pattern: "/api/envs/{id}/recovery-points", Methods: []string{"POST"}, Handler: s.createRecoveryHTTP},
		{Pattern: "/api/envs/{id}/restore", Methods: []string{"POST"}, Handler: s.restoreRecoveryHTTP},
		{Pattern: "/api/recovery-points", Methods: []string{"GET"}, Handler: s.recoveryPointsHTTP},
		{Pattern: "/api/recovery-points/{id}", Methods: []string{"GET", "DELETE"}, Handler: s.recoveryPointHTTP},
	}
}
func (s *Server) createRecoveryHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := command.CopyContext(s.workCtx, r.Context())
	if failure := requireCommand(ctx); failure != nil {
		reply(w, model.RecoveryPoint{}, failure, 201)
		return
	}
	request, ok := api.ReadJSON[api.CheckpointRequest](w, r)
	if !ok {
		return
	}
	point, failure := s.createRecoveryPoint(ctx, r.PathValue("id"), request.ID)
	reply(w, point, failure, 201)
}
func (s *Server) restoreRecoveryHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := command.CopyContext(s.workCtx, r.Context())
	if failure := requireCommand(ctx); failure != nil {
		reply(w, api.EnvDetail{}, failure, 202)
		return
	}
	request, ok := api.ReadJSON[api.RestorePointRequest](w, r)
	if !ok {
		return
	}
	detail, failure := s.restoreRecoveryPoint(ctx, r.PathValue("id"), request)
	reply(w, detail, failure, 202)
}
func (s *Server) recoveryPointsHTTP(w http.ResponseWriter, r *http.Request) {
	size, failure := pageSize(r)
	if failure != nil {
		reply(w, api.RecoveryPointPage{}, failure, 200)
		return
	}
	points, err := withDB(s, func(db *DB) ([]model.RecoveryPoint, error) { return db.ListRecoveryPoints(r.Context()) })
	if err != nil {
		reply(w, api.RecoveryPointPage{}, internal(err), 200)
		return
	}
	page := api.RecoveryPointPage{Points: []model.RecoveryPoint{}, MaxPageSize: 100}
	for _, point := range points {
		if point.ID <= r.URL.Query().Get("after") || point.State == "deleted" || r.URL.Query().Get("environment") != "" && point.EnvironmentID != r.URL.Query().Get("environment") {
			continue
		}
		if len(page.Points) == size {
			page.NextToken = page.Points[len(page.Points)-1].ID
			break
		}
		page.Points = append(page.Points, point)
	}
	reply(w, page, nil, 200)
}
func (s *Server) recoveryPointHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" || r.Method == "HEAD" {
		point, failure := s.recoveryPoint(r.Context(), r.PathValue("id"))
		reply(w, point, failure, 200)
		return
	}
	ctx := command.CopyContext(s.workCtx, r.Context())
	if failure := requireCommand(ctx); failure != nil {
		reply(w, model.Unit{}, failure, 200)
		return
	}
	reply(w, model.Unit{}, s.deleteRecoveryPoint(ctx, r.PathValue("id")), 200)
}
