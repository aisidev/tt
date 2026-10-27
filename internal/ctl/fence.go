package ctl

import (
	"net/http"
	"strings"
	"time"

	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
)

// fenceHTTP advances receiver ownership without changing guest data, including
// for an absent subject. A late create cannot follow an acknowledged withdrawal.
func (s *Server) fenceHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := command.CopyContext(s.workCtx, r.Context())
	if failure := requireCommand(ctx); failure != nil {
		reply(w, model.Unit{}, failure, 200)
		return
	}
	id := r.PathValue("id")
	var hostIDs []string
	if strings.HasPrefix(r.URL.Path, "/api/volumes/") {
		v, err := withDB(s, func(db *DB) (*model.Volume, error) { return db.GetVolume(ctx, id) })
		if err != nil {
			reply(w, model.Unit{}, internal(err), 200)
			return
		}
		if v != nil && v.State != "deleted" {
			hostIDs = append(hostIDs, v.HostID)
		}
	} else {
		vms, err := withDB(s, func(db *DB) ([]model.Vm, error) { return db.VMsByEnv(ctx, id) })
		if err != nil {
			reply(w, model.Unit{}, internal(err), 200)
			return
		}
		seen := map[string]bool{}
		for _, vm := range vms {
			if !seen[vm.HostID] {
				seen[vm.HostID] = true
				hostIDs = append(hostIDs, vm.HostID)
			}
		}
	}
	client, err := s.clientFactory(30 * time.Second)
	if err != nil {
		reply(w, model.Unit{}, internal(err), 200)
		return
	}
	for _, hostID := range hostIDs {
		host, failure := s.diskHost(ctx, hostID)
		if failure != nil {
			reply(w, model.Unit{}, failure, 200)
			return
		}
		_, err = diskCall[struct {
			Fenced bool `json:"fenced"`
		}](ctx, client, host, "POST", "/api/subjects/"+id+"/fence", struct{}{})
		if err != nil {
			reply(w, model.Unit{}, internal(err), 200)
			return
		}
	}
	reply(w, model.Unit{}, nil, 200)
}
