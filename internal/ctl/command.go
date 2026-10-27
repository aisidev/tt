package ctl

import (
	"context"
	"encoding/json"
	"github.com/aisidev/tt/internal/command"
	"net/http"
	"strings"
)

type commandEnvelope = command.Envelope

func (s *Server) commandMiddleware(next http.Handler) http.Handler {
	return s.commandJournal.Wrap(next, func(r *http.Request) bool {
		return strings.HasPrefix(r.URL.Path, "/api/control/") || strings.HasPrefix(r.URL.Path, "/api/envs") || strings.HasPrefix(r.URL.Path, "/api/vms/") || strings.HasPrefix(r.URL.Path, "/api/volumes") || strings.HasPrefix(r.URL.Path, "/api/volume-snapshots") || strings.HasPrefix(r.URL.Path, "/api/recovery-points") || strings.HasPrefix(r.URL.Path, "/api/images/")
	}, func(r *http.Request, body []byte) (string, error) {
		if strings.HasPrefix(r.URL.Path, "/api/control/") {
			return s.commandJournal.ControlSubject()
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) >= 2 && (parts[1] == "volumes" || parts[1] == "volume-snapshots" || parts[1] == "recovery-points" || parts[1] == "images") {
			if len(parts) >= 3 {
				return parts[2], nil
			}
			var spec struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(body, &spec) != nil {
				return "", nil
			}
			return spec.ID, nil
		}

		if len(parts) >= 3 {
			if parts[1] == "envs" {
				return parts[2], nil
			}
			s.dbMu.Lock()
			defer s.dbMu.Unlock()
			vm, err := s.db.GetVM(r.Context(), parts[2])
			if err != nil {
				return "", err
			}
			if vm != nil {
				return vm.EnvID, nil
			}
			return "", nil
		}
		var req struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(body, &req) != nil {
			return "", nil
		}
		return req.ID, nil
	})
}

// nextObservation issues a durable sample sequence while the caller holds dbMu.
// It orders complete snapshots, including repeated stop/start cycles and restarts.
func (d *DB) nextObservation(ctx context.Context) (uint64, error) {
	var revision uint64
	err := d.q.QueryRowContext(ctx, "UPDATE observation_clock SET revision=revision+1 WHERE id=1 AND revision<9223372036854775807 RETURNING revision").Scan(&revision)
	return revision, err
}
