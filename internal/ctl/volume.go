package ctl

import (
	"context"
	"fmt"
	"github.com/aisidev/tt/internal/capability"
	"slices"
	"sort"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
)

func diskAPIError(class, code, message string) *APIError {
	return internal(&api.BoundaryError{Class: class, Code: code, Message: message})
}
func diskCall[T any](ctx context.Context, c *agentClient, host model.Host, method, path string, body any) (T, error) {
	var zero T
	response, err := c.request(ctx, method, host.Addr, path, body, nil)
	if err != nil {
		return zero, err
	}
	status := response.StatusCode
	result, err := decodeResponse[T](response)
	if err != nil {
		return zero, err
	}
	if result.Failure != nil {
		return zero, result.Failure
	}
	if !successful(status) || !result.OK {
		return zero, &api.BoundaryError{Class: "unknown", Code: "disk_executor_unavailable", Message: "disk executor did not establish the result"}
	}
	if result.Data == nil {
		return zero, &api.BoundaryError{Class: "unknown", Code: "disk_executor_unavailable", Message: "disk executor returned no data"}
	}
	return *result.Data, nil
}
func (s *Server) diskHost(ctx context.Context, id string) (model.Host, *APIError) {
	host, err := withDB(s, func(db *DB) (*model.Host, error) { return db.GetHost(ctx, id) })
	if err != nil {
		return model.Host{}, internal(err)
	}
	if host == nil || host.State != model.HostOnline {
		return model.Host{}, diskAPIError("transient", "host_unavailable", "disk locality is unavailable; reservation retained")
	}
	if !host.StorageFeatures.SupportsVolumes() {
		return model.Host{}, diskAPIError("terminal", "unimplemented", "host does not negotiate persistent disks")
	}
	return *host, nil
}
func (s *Server) getVolume(ctx context.Context, id string) (model.Volume, *APIError) {
	known, err := withDB(s, func(db *DB) (*model.Volume, error) { return db.GetVolume(ctx, id) })
	if err != nil {
		return model.Volume{}, internal(err)
	}
	if known == nil {
		return model.Volume{}, diskAPIError("terminal", "not_found", "volume not found")
	}
	if known.State == "deleted" {
		return *known, nil
	}
	host, failure := s.diskHost(ctx, known.HostID)
	if failure != nil {
		return model.Volume{}, failure
	}
	client, err := s.clientFactory(5 * time.Second)
	if err != nil {
		return model.Volume{}, internal(err)
	}
	observed, err := diskCall[model.Volume](ctx, client, host, "GET", "/api/volumes/"+id, nil)
	if err != nil {
		return model.Volume{}, internal(err)
	}
	if observed.ID != known.ID || observed.HostID != known.HostID || observed.Generation != known.Generation || observed.Owner != known.Owner {
		return model.Volume{}, diskAPIError("unknown", "identity_mismatch", "disk observation does not match its admitted owner")
	}
	_, err = withDB(s, func(db *DB) (model.Unit, error) {
		// A read cannot overwrite a later serialized disk mutation.
		latest, e := db.GetVolume(ctx, id)
		if e != nil {
			return model.Unit{}, e
		}
		if latest == nil {
			return model.Unit{}, fmt.Errorf("volume catalog missing")
		}
		if latest.State == "deleted" {
			observed = *latest
			return model.Unit{}, nil
		}
		if observed.Revision >= latest.Revision {
			if latest.PendingSizeMiB > 0 && (observed.SizeMiB != latest.PendingSizeMiB || observed.State == "growing") {
				observed.SizeMiB = max(observed.SizeMiB, latest.PendingSizeMiB)
				observed.PendingSizeMiB = latest.PendingSizeMiB
				observed.State = "growing"
				observed.Phase = "growing"
			}
			if latest.State == "deleting" && observed.State != "deleted" {
				observed.State = "deleting"
				observed.Phase = "deleting"
			}
			return model.Unit{}, db.PutVolume(ctx, &observed)
		}
		observed = *latest
		return model.Unit{}, nil
	})
	if err != nil {
		return model.Volume{}, internal(err)
	}
	return observed, nil
}
func (s *Server) createVolume(ctx context.Context, spec model.VolumeSpec) (model.Volume, *APIError) {
	envelope, ok := command.FromContext(ctx)
	if !ok || envelope.Subject != spec.ID || envelope.Generation != spec.Generation {
		return model.Volume{}, diskAPIError("terminal", "missing_identity", "volume creation requires matching fenced identity")
	}
	if model.ValidateName(spec.ID, "volume") != nil || spec.Generation == 0 || spec.SizeMiB == 0 || spec.SizeMiB > 1<<20 {
		return model.Volume{}, diskAPIError("terminal", "invalid_volume", "invalid volume identity or size")
	}
	s.dbMu.Lock()
	known, err := s.db.GetVolume(ctx, spec.ID)
	if err != nil {
		s.dbMu.Unlock()
		return model.Volume{}, internal(err)
	}
	if known == nil {
		if spec.FromSnapshot != "" {
			point, e := s.db.GetVolumeSnapshot(ctx, spec.FromSnapshot)
			if e != nil {
				s.dbMu.Unlock()
				return model.Volume{}, internal(e)
			}
			if point == nil || point.State != "ready" {
				s.dbMu.Unlock()
				return model.Volume{}, diskAPIError("state", "snapshot_unavailable", "source checkpoint is unavailable")
			}
			if spec.HostID != "" && spec.HostID != point.HostID {
				s.dbMu.Unlock()
				return model.Volume{}, diskAPIError("state", "wrong_locality", "checkpoint locality cannot change")
			}
			spec.HostID = point.HostID
			spec.Engine = point.Source.Engine
			spec.Image = point.Source.Image
		}
		hosts, e := s.db.ListHosts(ctx)
		if e != nil {
			s.dbMu.Unlock()
			return model.Volume{}, internal(e)
		}
		sort.SliceStable(hosts, func(i, j int) bool {
			if hosts[i].Storage != hosts[j].Storage {
				return hosts[i].Storage == model.Zvol
			}
			return hosts[i].Resource.DiskFree() < hosts[j].Resource.DiskFree()
		})
		format := "raw"
		var selected *model.Host
		desired := model.Volume{ID: spec.ID, Owner: spec.Owner, Generation: spec.Generation, SizeMiB: spec.SizeMiB, Engine: spec.Engine, Image: spec.Image, Retain: spec.Retain, SourceSnapshot: spec.FromSnapshot, State: "creating", Phase: "reserved", CreatedAt: s.now()}
		for i := range hosts {
			host := &hosts[i]
			if host.State != model.HostOnline || !host.StorageFeatures.SupportsVolumes() || spec.HostID != "" && spec.HostID != host.ID || !host.Resource.CanFit(0, 0, desired.ReservedMiB()) {
				continue
			}
			if !volumeEngineHost(*host, spec, spec.FromSnapshot == "") {
				continue
			}
			selected = host
			break
		}
		if selected == nil {
			s.dbMu.Unlock()
			return model.Volume{}, diskAPIError("transient", "capacity_exceeded", "no qualified host can admit the disk")
		}
		if spec.Engine == model.Qemu && selected.Storage == model.File {
			format = "qcow2"
		}
		desired.Format = format
		desired.HostID = selected.ID
		selected.Resource.DiskUsed = model.SatAdd(selected.Resource.DiskUsed, desired.ReservedMiB())
		err = s.db.transaction(ctx, "volume admission", func(db *DB) error {
			if e := db.PutVolume(ctx, &desired); e != nil {
				return e
			}
			return db.PutHost(ctx, selected)
		})
		if err != nil {
			s.dbMu.Unlock()
			return model.Volume{}, internal(err)
		}
		known = &desired
	} else if known.Generation != spec.Generation || known.Owner != spec.Owner || known.SizeMiB != spec.SizeMiB || known.SourceSnapshot != spec.FromSnapshot || (spec.HostID != "" && spec.HostID != known.HostID) || (spec.FromSnapshot == "" && (spec.Image != known.Image || spec.Engine != known.Engine)) {
		s.dbMu.Unlock()
		return model.Volume{}, diskAPIError("terminal", "fingerprint_conflict", "volume identity has different creation parameters")
	}
	s.dbMu.Unlock()
	host, failure := s.diskHost(ctx, known.HostID)
	if failure != nil {
		return model.Volume{}, failure
	}
	spec.HostID = known.HostID
	spec.Engine = known.Engine
	spec.Image = known.Image
	client, err := s.clientFactory(360 * time.Second)
	if err != nil {
		return model.Volume{}, internal(err)
	}
	observed, err := diskCall[model.Volume](ctx, client, host, "POST", "/api/volumes", spec)
	if err != nil {
		return model.Volume{}, internal(err)
	}
	if observed.ID != known.ID || observed.HostID != known.HostID || observed.Generation != known.Generation {
		return model.Volume{}, diskAPIError("unknown", "identity_mismatch", "disk executor returned a different identity")
	}
	_, err = withDB(s, func(db *DB) (model.Unit, error) { return model.Unit{}, db.PutVolume(ctx, &observed) })
	return observed, internal(err)
}
func (s *Server) mutateVolume(ctx context.Context, id, method, suffix string, body any) (model.Volume, *APIError) {
	known, err := withDB(s, func(db *DB) (*model.Volume, error) { return db.GetVolume(ctx, id) })
	if err != nil {
		return model.Volume{}, internal(err)
	}
	if known == nil {
		return model.Volume{}, diskAPIError("terminal", "not_found", "volume not found")
	}
	host, failure := s.diskHost(ctx, known.HostID)
	if failure != nil {
		return model.Volume{}, failure
	}
	client, err := s.clientFactory(360 * time.Second)
	if err != nil {
		return model.Volume{}, internal(err)
	}
	if method == "PATCH" {
		update := body.(model.VolumeUpdate)
		if update.SizeMiB != nil && *update.SizeMiB > known.SizeMiB {
			_, err = withDB(s, func(db *DB) (model.Unit, error) {
				current, e := db.GetVolume(ctx, id)
				if e != nil {
					return model.Unit{}, e
				}
				if current == nil {
					return model.Unit{}, fmt.Errorf("volume catalog missing")
				}
				previous := current.ReservedMiB()
				current.SizeMiB = *update.SizeMiB
				current.PendingSizeMiB = *update.SizeMiB
				delta := model.SatSub(current.ReservedMiB(), previous)
				h, e := db.GetHost(ctx, current.HostID)
				if e != nil {
					return model.Unit{}, e
				}
				if h == nil || !h.Resource.CanFit(0, 0, delta) {
					return model.Unit{}, &api.BoundaryError{Class: "transient", Code: "capacity_exceeded", Message: "insufficient disk capacity"}
				}
				current.State = "growing"
				current.Phase = "growing"
				h.Resource.DiskUsed = model.SatAdd(h.Resource.DiskUsed, delta)
				return model.Unit{}, db.transaction(ctx, "volume growth admission", func(tx *DB) error {
					if e := tx.PutVolume(ctx, current); e != nil {
						return e
					}
					return tx.PutHost(ctx, h)
				})
			})
			if err != nil {
				return model.Volume{}, internal(err)
			}
		}
		observed, err := diskCall[model.Volume](ctx, client, host, method, "/api/volumes/"+id, body)
		if err != nil {
			return model.Volume{}, internal(err)
		}
		_, err = withDB(s, func(db *DB) (model.Unit, error) { return model.Unit{}, db.PutVolume(ctx, &observed) })
		return observed, internal(err)
	}
	if method == "DELETE" {
		known.State = "deleting"
		known.Phase = "deleting"
		if _, err = withDB(s, func(db *DB) (model.Unit, error) { return model.Unit{}, db.PutVolume(ctx, known) }); err != nil {
			return model.Volume{}, internal(err)
		}
	}
	// Empty successful action responses carry no data; read the identity afterwards.
	response, err := client.request(ctx, method, host.Addr, "/api/volumes/"+id+suffix, body, nil)
	if err != nil {
		return model.Volume{}, internal(err)
	}
	status := response.StatusCode
	result, err := decodeResponse[model.Unit](response)
	if err != nil {
		return model.Volume{}, internal(err)
	}
	if result.Failure != nil {
		return model.Volume{}, internal(result.Failure)
	}
	if !successful(status) || !result.OK {
		return model.Volume{}, diskAPIError("unknown", "disk_executor_unavailable", "disk executor did not confirm the action")
	}
	observed, failure := s.getVolume(ctx, id)
	if (method == "DELETE" || (method == "POST" && suffix == "/import/abort")) && failure != nil && failure.Failure != nil && failure.Failure.Class == "terminal" && failure.Failure.Code == "not_found" {
		// A successful modern delete ACK proves fenced physical absence, even
		// if the executor never admitted a row for the controller's plan.
		known.State = "deleted"
		known.Phase = "deleted"
		known.Revision++
		_, err = withDB(s, func(db *DB) (model.Unit, error) { return model.Unit{}, db.PutVolume(ctx, known) })
		return *known, internal(err)
	}
	return observed, failure
}
func (s *Server) snapshotVolume(ctx context.Context, id, checkpoint string) (model.VolumeSnapshot, *APIError) {
	source, failure := s.getVolume(ctx, id)
	if failure != nil {
		return model.VolumeSnapshot{}, failure
	}
	if model.ValidateName(checkpoint, "checkpoint") != nil {
		return model.VolumeSnapshot{}, diskAPIError("terminal", "invalid_snapshot", "invalid checkpoint identity")
	}
	s.dbMu.Lock()
	known, err := s.db.GetVolumeSnapshot(ctx, checkpoint)
	if err == nil && known == nil {
		host, e := s.db.GetHost(ctx, source.HostID)
		if e != nil {
			err = e
		} else if host == nil || !host.Resource.CanFit(0, 0, source.ReservedMiB()) {
			err = &api.BoundaryError{Class: "transient", Code: "capacity_exceeded", Message: "insufficient checkpoint capacity"}
		} else {
			known = &model.VolumeSnapshot{ID: checkpoint, VolumeID: id, Owner: source.Owner, HostID: source.HostID, Generation: source.Generation, Source: source, State: "creating", Phase: "reserved", CreatedAt: s.now()}
			host.Resource.DiskUsed = model.SatAdd(host.Resource.DiskUsed, source.ReservedMiB())
			err = s.db.transaction(ctx, "checkpoint admission", func(tx *DB) error {
				if e := tx.PutVolumeSnapshot(ctx, known); e != nil {
					return e
				}
				return tx.PutHost(ctx, host)
			})
		}
	}
	s.dbMu.Unlock()
	if err != nil {
		return model.VolumeSnapshot{}, internal(err)
	}
	if known.VolumeID != id {
		return model.VolumeSnapshot{}, diskAPIError("terminal", "fingerprint_conflict", "checkpoint belongs to another disk")
	}
	host, failure := s.diskHost(ctx, source.HostID)
	if failure != nil {
		return model.VolumeSnapshot{}, failure
	}
	client, err := s.clientFactory(360 * time.Second)
	if err != nil {
		return model.VolumeSnapshot{}, internal(err)
	}
	point, err := diskCall[model.VolumeSnapshot](ctx, client, host, "POST", "/api/volumes/"+id+"/snapshots", api.SnapshotVolumeRequest{ID: checkpoint})
	if err != nil {
		return model.VolumeSnapshot{}, internal(err)
	}
	if point.ID != checkpoint || point.VolumeID != id || point.HostID != known.HostID {
		return model.VolumeSnapshot{}, diskAPIError("unknown", "identity_mismatch", "checkpoint executor returned a different identity")
	}
	_, err = withDB(s, func(db *DB) (model.Unit, error) { return model.Unit{}, db.PutVolumeSnapshot(ctx, &point) })
	return point, internal(err)
}
func (s *Server) getVolumeSnapshot(ctx context.Context, id string) (model.VolumeSnapshot, *APIError) {
	known, err := withDB(s, func(db *DB) (*model.VolumeSnapshot, error) { return db.GetVolumeSnapshot(ctx, id) })
	if err != nil {
		return model.VolumeSnapshot{}, internal(err)
	}
	if known == nil {
		return model.VolumeSnapshot{}, diskAPIError("terminal", "not_found", "checkpoint not found")
	}
	if known.State == "deleted" {
		return *known, nil
	}
	host, failure := s.diskHost(ctx, known.HostID)
	if failure != nil {
		return model.VolumeSnapshot{}, failure
	}
	client, err := s.clientFactory(5 * time.Second)
	if err != nil {
		return model.VolumeSnapshot{}, internal(err)
	}
	point, err := diskCall[model.VolumeSnapshot](ctx, client, host, "GET", "/api/volume-snapshots/"+id, nil)
	if err != nil {
		return model.VolumeSnapshot{}, internal(err)
	}
	if point.ID != known.ID || point.VolumeID != known.VolumeID || point.HostID != known.HostID {
		return model.VolumeSnapshot{}, diskAPIError("unknown", "identity_mismatch", "checkpoint observation has a different identity")
	}
	_, err = withDB(s, func(db *DB) (model.Unit, error) {
		latest, e := db.GetVolumeSnapshot(ctx, id)
		if e != nil {
			return model.Unit{}, e
		}
		if latest == nil {
			return model.Unit{}, fmt.Errorf("checkpoint catalog missing")
		}
		if latest.State == "deleted" || latest.Revision > point.Revision {
			point = *latest
			return model.Unit{}, nil
		}
		if latest.State == "deleting" && point.State != "deleted" {
			point.State = "deleting"
			point.Phase = "deleting"
		}
		return model.Unit{}, db.PutVolumeSnapshot(ctx, &point)
	})
	return point, internal(err)
}
func (s *Server) deleteVolumeSnapshot(ctx context.Context, id string) *APIError {
	known, failure := s.getVolumeSnapshot(ctx, id)
	if failure != nil {
		return failure
	}
	if known.State == "deleted" {
		return nil
	}
	host, failure := s.diskHost(ctx, known.HostID)
	if failure != nil {
		return failure
	}
	client, err := s.clientFactory(360 * time.Second)
	if err != nil {
		return internal(err)
	}
	response, err := client.request(ctx, "DELETE", host.Addr, "/api/volume-snapshots/"+id, nil, nil)
	if err != nil {
		return internal(err)
	}
	status := response.StatusCode
	result, err := decodeResponse[model.Unit](response)
	if err != nil {
		return internal(err)
	}
	if result.Failure != nil {
		return internal(result.Failure)
	}
	if !successful(status) || !result.OK {
		return diskAPIError("unknown", "disk_executor_unavailable", "checkpoint destruction was not confirmed")
	}
	_, failure = s.getVolumeSnapshot(ctx, id)
	return failure
}

func requireCommand(ctx context.Context) *APIError {
	if _, ok := command.FromContext(ctx); !ok {
		return diskAPIError("terminal", "missing_identity", "disk mutations require command identity")
	}
	return nil
}

func volumeEngineHost(host model.Host, spec model.VolumeSpec, requireImage bool) bool {
	if spec.Engine == "" {
		return spec.Image == ""
	}
	if !slices.Contains(host.Engines, spec.Engine) || capability.RequireHost(host, spec.Engine, []capability.Feature{capability.Run, capability.DiskSize}) != nil {
		return false
	}
	if !requireImage {
		return true
	}
	base, known := host.ImageSizes[spec.Image]
	return slices.Contains(host.Images, spec.Image) && known && base > 0 && spec.SizeMiB >= base
}
