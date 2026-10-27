package ctl

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
)

// A successful pull must be immediately schedulable. Publish only the observed
// image catalog and conservatively increased disk usage; a full fleet snapshot
// cannot overwrite unrelated in-flight VM intent here.
func (s *Server) confirmImageCatalog(ctx context.Context, client *agentClient, target imageTarget, id string, rootMiB uint32) error {
	response, err := client.request(ctx, "GET", target.Addr, "/api/info", nil, nil)
	if err != nil {
		return err
	}
	status := response.StatusCode
	result, err := decodeResponse[api.AgentInfo](response)
	if err != nil {
		return err
	}
	if result.Failure != nil {
		return result.Failure
	}
	if !successful(status) || !result.OK || result.Data == nil || result.Data.HostID != target.ID {
		return &api.BoundaryError{Class: "unknown", Code: "identity_mismatch", Message: "image receiver observation changed"}
	}
	info := result.Data
	if !slices.Contains(info.Images, id) || info.ImageSizes[id] == 0 || rootMiB > 0 && info.ImageSizes[id] != rootMiB {
		return &api.BoundaryError{Class: "unknown", Code: "image_not_ready", Message: "verified cache is not yet advertised with its expected capacity"}
	}
	_, err = withDB(s, func(db *DB) (model.Unit, error) {
		host, err := db.GetHost(ctx, target.ID)
		if err != nil {
			return model.Unit{}, err
		}
		if host == nil || host.Addr != target.Addr {
			return model.Unit{}, &api.BoundaryError{Class: "state", Code: "image_host_changed", Message: "original image receiver is unavailable"}
		}
		host.Images = info.Images
		host.ImageSizes = info.ImageSizes
		host.Resource.DiskUsed = max(host.Resource.DiskUsed, info.Resource.DiskUsed)
		s.revision.Add(1)
		return model.Unit{}, db.PutHost(ctx, host)
	})
	return err
}

type imageTarget struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}
type imagePlan struct {
	ID       string                   `json:"id"`
	Request  api.ArchiveImportRequest `json:"request"`
	Targets  []imageTarget            `json:"targets"`
	LastKey  string                   `json:"last_key"`
	Deleting bool                     `json:"deleting"`
}

// The complete target set precedes any pull. Retry and retirement keep the
// original receiver identities, including offline hosts; catalog loss cannot
// turn an unfinished pull into verified absence.
func (s *Server) planImage(ctx context.Context, id string, body api.ArchiveImportRequest, deleting bool) (imagePlan, error) {
	return withDB(s, func(db *DB) (imagePlan, error) {
		old, err := queryOne[imagePlan](ctx, db.q, "SELECT data FROM image_catalog WHERE id=?1", id)
		if err != nil {
			return imagePlan{}, err
		}
		if old != nil {
			envelope, identified := command.FromContext(ctx)
			if !deleting && !old.Deleting && old.Request == body && identified && envelope.Key != old.LastKey {
				hosts, e := db.ListHosts(ctx)
				if e != nil {
					return imagePlan{}, e
				}
				for _, host := range hosts {
					if host.State != model.HostOnline || !host.StorageFeatures.SupportsVolumes() || !host.StorageFeatures.ImagePull {
						continue
					}
					if body.Spec.Engine != "" && (!host.StorageFeatures.ImageCatalog || !volumeEngineHost(host, body.Spec, false)) {
						continue
					}
					found := false
					for _, target := range old.Targets {
						if target.ID == host.ID {
							found = true
							if target.Addr != host.Addr {
								return imagePlan{}, &api.BoundaryError{Class: "state", Code: "image_host_changed", Message: "original image receiver is unavailable"}
							}
						}
					}
					if !found {
						old.Targets = append(old.Targets, imageTarget{host.ID, host.Addr})
					}
					if len(old.Targets) > 4096 {
						return imagePlan{}, &api.BoundaryError{Class: "terminal", Code: "size_exceeded", Message: "image targets exceed budget"}
					}
				}
				old.LastKey = envelope.Key
				if e = putRecord(ctx, db.q, "UPDATE image_catalog SET data=?2 WHERE id=?1", "image plan", old, id); e != nil {
					return imagePlan{}, e
				}
			}
			if !deleting && (old.Deleting || old.Request != body) {
				return imagePlan{}, &api.BoundaryError{Class: "state", Code: "image_conflict", Message: "image profile or retirement changed"}
			}
			if deleting && !old.Deleting {
				old.Deleting = true
				err = putRecord(ctx, db.q, "UPDATE image_catalog SET data=?2 WHERE id=?1", "image plan", old, id)
			}
			return *old, err
		}
		envelope, _ := command.FromContext(ctx)
		plan := imagePlan{LastKey: envelope.Key, ID: id, Request: body, Targets: []imageTarget{}, Deleting: deleting}
		if deleting {
			// Absence of this journal proves this controller never dispatched a pull.
			// External/unrecorded cache bytes remain recovery-inventory quarantine.
			return plan, nil
		}
		hosts, err := db.ListHosts(ctx)
		if err != nil {
			return plan, err
		}
		if len(hosts) > 4096 {
			return plan, &api.BoundaryError{Class: "terminal", Code: "size_exceeded", Message: "image target set exceeds budget"}
		}
		for _, host := range hosts {
			if host.State != model.HostOnline || !host.StorageFeatures.SupportsVolumes() || !host.StorageFeatures.ImagePull {
				continue
			}
			if body.Spec.Engine != "" && (!host.StorageFeatures.ImageCatalog || !volumeEngineHost(host, body.Spec, false)) {
				continue
			}
			plan.Targets = append(plan.Targets, imageTarget{host.ID, host.Addr})
		}
		if len(plan.Targets) == 0 {
			return plan, &api.BoundaryError{Class: "transient", Code: "host_unavailable", Message: "no qualified image receiver is online"}
		}
		err = putRecord(ctx, db.q, "INSERT INTO image_catalog(id,data) VALUES(?1,?2)", "image plan", plan, id)
		return plan, err
	})
}

func (s *Server) evictImageHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := command.CopyContext(s.workCtx, r.Context())
	if failure := requireCommand(ctx); failure != nil {
		reply(w, model.Unit{}, failure, 200)
		return
	}
	plan, err := s.planImage(ctx, r.PathValue("id"), api.ArchiveImportRequest{}, true)
	if err != nil {
		reply(w, model.Unit{}, internal(err), 200)
		return
	}
	client, err := s.clientFactory(30 * time.Minute)
	if err != nil {
		reply(w, model.Unit{}, internal(err), 200)
		return
	}
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
		response, e := client.request(ctx, "DELETE", host.Addr, "/api/images/"+plan.ID+"/cache", nil, nil)
		if e != nil {
			reply(w, model.Unit{}, internal(e), 200)
			return
		}
		status := response.StatusCode
		result, e := decodeResponse[model.Unit](response)
		if e == nil && result.Failure != nil {
			e = result.Failure
		}
		if e == nil && (!successful(status) || !result.OK) {
			e = &api.BoundaryError{Class: "unknown", Code: "archive_incomplete", Message: "image cache removal unconfirmed"}
		}
		if e != nil {
			reply(w, model.Unit{}, internal(e), 200)
			return
		}
	}
	reply(w, model.Unit{}, nil, 200)
}

func (s *Server) fenceImageHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := command.CopyContext(s.workCtx, r.Context())
	if failure := requireCommand(ctx); failure != nil {
		reply(w, model.Unit{}, failure, 200)
		return
	}
	plan, err := withDB(s, func(db *DB) (*imagePlan, error) {
		return queryOne[imagePlan](ctx, db.q, "SELECT data FROM image_catalog WHERE id=?1", r.PathValue("id"))
	})
	if err != nil {
		reply(w, model.Unit{}, internal(err), 200)
		return
	}
	if plan == nil {
		reply(w, model.Unit{}, nil, 200)
		return
	}
	client, err := s.clientFactory(30 * time.Second)
	if err != nil {
		reply(w, model.Unit{}, internal(err), 200)
		return
	}
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
		_, err = diskCall[struct {
			Fenced bool `json:"fenced"`
		}](ctx, client, host, "POST", "/api/subjects/"+plan.ID+"/fence", struct{}{})
		if err != nil {
			reply(w, model.Unit{}, internal(err), 200)
			return
		}
	}
	reply(w, model.Unit{}, nil, 200)
}
