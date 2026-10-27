package ctl

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
)

func (d *DB) PutRecoveryPoint(ctx context.Context, p *model.RecoveryPoint) error {
	return putRecord(ctx, d.q, "INSERT OR REPLACE INTO recovery_points(id,data) VALUES(?1,?2)", "recovery point", p, p.ID)
}
func (d *DB) GetRecoveryPoint(ctx context.Context, id string) (*model.RecoveryPoint, error) {
	p, err := queryOne[model.RecoveryPoint](ctx, d.q, "SELECT data FROM recovery_points WHERE id=?1", id)
	if err == nil && p != nil && (p.ID != id || p.EnvironmentID == "" || p.Generation == 0 || p.State == "") {
		return nil, &api.BoundaryError{Class: "terminal", Code: "malformed", Message: "malformed recovery manifest"}
	}
	return p, err
}
func (d *DB) ListRecoveryPoints(ctx context.Context) ([]model.RecoveryPoint, error) {
	return queryAll[model.RecoveryPoint](ctx, d.q, "SELECT data FROM recovery_points ORDER BY id")
}
func (s *Server) saveRecoveryPoint(ctx context.Context, p *model.RecoveryPoint) *APIError {
	p.Revision++
	_, err := withDB(s, func(db *DB) (model.Unit, error) { return model.Unit{}, db.PutRecoveryPoint(ctx, p) })
	return internal(err)
}
func (s *Server) recoveryPoint(ctx context.Context, id string) (model.RecoveryPoint, *APIError) {
	p, err := withDB(s, func(db *DB) (*model.RecoveryPoint, error) { return db.GetRecoveryPoint(ctx, id) })
	if err != nil {
		return model.RecoveryPoint{}, internal(err)
	}
	if p == nil {
		return model.RecoveryPoint{}, diskAPIError("terminal", "not_found", "recovery point not found")
	}
	return *p, nil
}
func recoveryContext(ctx context.Context, point string) context.Context {
	envelope, _ := command.FromContext(ctx)
	envelope.Group = point
	return command.WithContext(ctx, envelope)
}
func checkpointDiskID(point, volume string, generation uint64) string {
	sum := sha256.Sum256([]byte(point + "/" + volume))
	return fmt.Sprintf("point-%x-g%d", sum[:16], generation)
}
func (s *Server) createRecoveryPoint(ctx context.Context, environment, id string) (model.RecoveryPoint, *APIError) {
	envelope, ok := command.FromContext(ctx)
	if !ok || envelope.Subject != environment || model.ValidateName(id, "recovery point") != nil {
		return model.RecoveryPoint{}, diskAPIError("terminal", "missing_identity", "recovery requires a matching fenced identity")
	}
	operation, failure := s.beginOperation(environment)
	if failure != nil {
		return model.RecoveryPoint{}, failure
	}
	defer operation.Close()
	point, err := withDB(s, func(db *DB) (*model.RecoveryPoint, error) { return db.GetRecoveryPoint(ctx, id) })
	if err != nil {
		return model.RecoveryPoint{}, internal(err)
	}
	if point != nil {
		if point.EnvironmentID != environment || point.Generation != envelope.Generation {
			return model.RecoveryPoint{}, diskAPIError("terminal", "fingerprint_conflict", "recovery point has a different source")
		}
		if point.State == "ready" {
			return *point, nil
		}
		if point.State != "creating" {
			return model.RecoveryPoint{}, diskAPIError("state", "snapshot_unavailable", "recovery point is not available")
		}
	} else {
		reader, e := s.clientFactory(5 * time.Second)
		if e != nil {
			return model.RecoveryPoint{}, internal(e)
		}
		s.refreshHosts(ctx, reader, &environment)
		if e = s.updateEnvState(ctx, environment); e != nil {
			return model.RecoveryPoint{}, internal(e)
		}
		detail, failure := s.environmentDetail(ctx, environment)
		if failure != nil {
			return model.RecoveryPoint{}, failure
		}
		if len(detail.VMs) != 1 || detail.Env.State != model.EnvStopped || detail.VMs[0].State != model.VmStopped {
			return model.RecoveryPoint{}, diskAPIError("state", "workspace_not_stopped", "recovery requires exactly one stopped VM")
		}
		vm := detail.VMs[0]
		if !vm.Options.RootManaged {
			return model.RecoveryPoint{}, diskAPIError("terminal", "unimplemented", "recovery requires persistent volumes")
		}
		point = &model.RecoveryPoint{ID: id, EnvironmentID: environment, Generation: envelope.Generation, HostID: vm.HostID, Owner: detail.Env.Owner, VM: vm, State: "creating", Phase: "freezing", CreatedAt: s.now(), Disks: []model.RecoveryDisk{}}
		if failure = s.saveRecoveryPoint(ctx, point); failure != nil {
			return model.RecoveryPoint{}, failure
		}
	}
	host, failure := s.diskHost(ctx, point.HostID)
	if failure != nil {
		return model.RecoveryPoint{}, failure
	}
	if !host.StorageFeatures.WorkspaceSnapshots {
		return model.RecoveryPoint{}, diskAPIError("terminal", "unimplemented", "host does not negotiate workspace checkpoints")
	}
	client, err := s.clientFactory(360 * time.Second)
	if err != nil {
		return model.RecoveryPoint{}, internal(err)
	}
	ctx = recoveryContext(ctx, id)
	if point.Phase == "freezing" {
		freezeEnvelope, _ := command.FromContext(ctx)
		freezeEnvelope.Key += "/freeze"
		frozen, e := diskCall[model.FrozenVM](command.WithContext(ctx, freezeEnvelope), client, host, "POST", "/api/vms/"+point.VM.ID+"/checkpoint/freeze", api.CheckpointRequest{ID: id})
		if e != nil {
			return model.RecoveryPoint{}, internal(e)
		}
		if frozen.VM.ID != point.VM.ID || frozen.PointID != id || len(frozen.Volumes) < 1 {
			return model.RecoveryPoint{}, diskAPIError("unknown", "identity_mismatch", "checkpoint freeze returned a different disk set")
		}
		point.VM = frozen.VM
		point.Disks = []model.RecoveryDisk{}
		for _, volume := range frozen.Volumes {
			role := "data"
			if volume.ID == frozen.VM.Options.RootVolumeID {
				role = "root"
			}
			point.Disks = append(point.Disks, model.RecoveryDisk{VolumeID: volume.ID, SnapshotID: checkpointDiskID(id, volume.ID, volume.Generation), Generation: volume.Generation, Role: role, Source: volume})
		}
		point.Phase = "disks"
		if failure = s.saveRecoveryPoint(ctx, point); failure != nil {
			return model.RecoveryPoint{}, failure
		}
	}
	if point.Phase == "disks" {
		for _, disk := range point.Disks {
			child := envelope
			child.Subject = disk.VolumeID
			child.Generation = disk.Generation
			child.Key += "/disk/" + disk.VolumeID
			child.Group = id
			if _, failure = s.snapshotVolume(command.WithContext(ctx, child), disk.VolumeID, disk.SnapshotID); failure != nil {
				return model.RecoveryPoint{}, failure
			}
		}
		point.Phase = "thawing"
		if failure = s.saveRecoveryPoint(ctx, point); failure != nil {
			return model.RecoveryPoint{}, failure
		}
	}
	thawEnvelope, _ := command.FromContext(ctx)
	thawEnvelope.Key += "/thaw"
	response, err := client.request(command.WithContext(ctx, thawEnvelope), "POST", host.Addr, "/api/vms/"+point.VM.ID+"/checkpoint/thaw", api.CheckpointRequest{ID: id}, nil)
	if err != nil {
		return model.RecoveryPoint{}, internal(err)
	}
	status := response.StatusCode
	result, err := decodeResponse[model.Unit](response)
	if err != nil {
		return model.RecoveryPoint{}, internal(err)
	}
	if result.Failure != nil {
		return model.RecoveryPoint{}, internal(result.Failure)
	}
	if !successful(status) || !result.OK {
		return model.RecoveryPoint{}, diskAPIError("unknown", "checkpoint_incomplete", "checkpoint writer fence remains retained")
	}
	point.State = "ready"
	point.Phase = "ready"
	point.Error = nil
	failure = s.saveRecoveryPoint(ctx, point)
	return *point, failure
}
func (s *Server) deleteRecoveryPoint(ctx context.Context, id string) *APIError {
	point, failure := s.recoveryPoint(ctx, id)
	if failure != nil {
		return failure
	}
	if point.State == "deleted" {
		return nil
	}
	if point.State == "creating" || point.Phase == "aborting" {
		return s.abortRecoveryPoint(ctx, &point)
	}
	if point.State != "ready" && point.State != "deleting" {
		return diskAPIError("state", "checkpoint_busy", "creating checkpoint must finish or be explicitly aborted")
	}
	point.State = "deleting"
	point.Phase = "deleting"
	if failure = s.saveRecoveryPoint(ctx, &point); failure != nil {
		return failure
	}
	envelope, ok := command.FromContext(ctx)
	if !ok {
		return diskAPIError("terminal", "missing_identity", "checkpoint deletion requires identity")
	}
	for _, disk := range point.Disks {
		child := envelope
		child.Subject = disk.SnapshotID
		child.Generation = disk.Generation
		child.Key += "/point/" + disk.SnapshotID
		if failure = s.deleteVolumeSnapshot(command.WithContext(ctx, child), disk.SnapshotID); failure != nil {
			return failure
		}
	}
	point.State = "deleted"
	point.Phase = "deleted"
	return s.saveRecoveryPoint(ctx, &point)
}

type restoreIntent struct {
	Target     string                       `json:"target"`
	Generation uint64                       `json:"generation"`
	Hash       string                       `json:"hash"`
	Point      model.RecoveryPoint          `json:"point"`
	Request    api.RestorePointRequest      `json:"request"`
	Volumes    model.List[model.VolumeSpec] `json:"volumes"`
}

func (s *Server) restoreRecoveryPoint(ctx context.Context, target string, request api.RestorePointRequest) (api.EnvDetail, *APIError) {
	envelope, ok := command.FromContext(ctx)
	if !ok || envelope.Subject != target {
		return api.EnvDetail{}, diskAPIError("terminal", "missing_identity", "restore requires the new VM identity")
	}
	raw, _ := json.Marshal(request)
	sum := sha256.Sum256(raw)
	hash := fmt.Sprintf("%x", sum)
	s.dbMu.Lock()
	intent, err := queryOne[restoreIntent](ctx, s.db.q, "SELECT data FROM recovery_restores WHERE id=?1", target)
	if err == nil && intent == nil {
		point, e := s.db.GetRecoveryPoint(ctx, request.PointID)
		if e != nil {
			err = e
		} else if point == nil || point.State != "ready" {
			err = &api.BoundaryError{Class: "state", Code: "snapshot_unavailable", Message: "recovery point is not ready"}
		} else {
			existing, e := s.db.GetEnv(ctx, target)
			if e != nil {
				err = e
			} else if existing != nil {
				err = &api.BoundaryError{Class: "state", Code: "env_exists", Message: "restore requires a new environment"}
			} else {
				intent = &restoreIntent{Target: target, Generation: envelope.Generation, Hash: hash, Point: *point, Request: request, Volumes: []model.VolumeSpec{}}
				for index, disk := range point.Disks {
					spec := model.VolumeSpec{ID: managedRootID("restore/"+target, index, envelope.Generation), Generation: envelope.Generation, HostID: point.HostID, Owner: request.Owner, SizeMiB: disk.Source.SizeMiB, Engine: disk.Source.Engine, Image: disk.Source.Image, Retain: disk.Source.Retain, FromSnapshot: disk.SnapshotID}
					intent.Volumes = append(intent.Volumes, spec)
				}
				data, _ := json.Marshal(intent)
				_, err = s.db.q.ExecContext(ctx, "INSERT INTO recovery_restores(id,data) VALUES(?1,?2)", target, string(data))
			}
		}
	}
	s.dbMu.Unlock()
	if err != nil {
		return api.EnvDetail{}, internal(err)
	}
	if intent.Hash != hash || intent.Generation != envelope.Generation {
		return api.EnvDetail{}, diskAPIError("terminal", "fingerprint_conflict", "restore identity has a different immutable plan")
	}
	for _, spec := range intent.Volumes {
		child := envelope
		child.Subject = spec.ID
		child.Generation = spec.Generation
		child.Key += "/volume/" + spec.ID
		if _, failure := s.createVolume(command.WithContext(ctx, child), spec); failure != nil {
			return api.EnvDetail{}, failure
		}
	}
	source := intent.Point.VM
	cpu, mem := source.CPU, source.Mem
	if request.CPU != nil {
		cpu = *request.CPU
	}
	if request.Mem != nil {
		mem = *request.Mem
	}
	spec := api.VmSpec{Image: source.Image, Engine: source.Engine, CPU: &cpu, Mem: &mem, HostID: intent.Point.HostID, InitialStopped: true, IsolatedNetwork: source.Options.IsolatedNetwork, DenyOutgoing: source.Options.DenyOutgoing, Ports: source.Options.Ports, SSHKeys: source.Options.SSHKeys, GuestConfig: request.GuestConfig}
	for index, disk := range intent.Point.Disks {
		if disk.Role == "root" {
			spec.RootVolumeID = intent.Volumes[index].ID
			size := intent.Volumes[index].SizeMiB
			spec.Disk = &size
		} else {
			spec.DataVolumeIDs = append(spec.DataVolumeIDs, intent.Volumes[index].ID)
		}
	}
	lifetime := uint64(0)
	return s.createEnvironment(ctx, api.CreateEnvReq{ID: target, Owner: request.Owner, Lifetime: &lifetime, VMs: model.List[api.VmSpec]{spec}})
}

func (s *Server) advanceRecoveryFence(ctx context.Context, client *agentClient, host model.Host, envelope command.Envelope, subject string, generation uint64, key string) error {
	envelope.Subject = subject
	envelope.Generation = generation
	envelope.Key += "/" + key
	_, err := withDB(s, func(db *DB) (model.Unit, error) {
		var highest uint64
		if e := db.q.QueryRowContext(ctx, "SELECT COALESCE((SELECT token FROM command_fences WHERE subject=?1),0)", subject).Scan(&highest); e != nil {
			return model.Unit{}, e
		}
		if highest > envelope.Token {
			return model.Unit{}, &api.BoundaryError{Class: "state", Code: "stale_fence", Message: "checkpoint ownership was superseded"}
		}
		_, e := db.q.ExecContext(ctx, "INSERT INTO command_fences(subject,token) VALUES(?1,?2) ON CONFLICT(subject) DO UPDATE SET token=excluded.token", subject, envelope.Token)
		return model.Unit{}, e
	})
	if err != nil {
		return err
	}
	_, err = diskCall[struct {
		Fenced bool `json:"fenced"`
	}](command.WithContext(ctx, envelope), client, host, "POST", "/api/subjects/"+subject+"/fence", struct{}{})
	return err
}
func (s *Server) abortRecoveryPoint(ctx context.Context, point *model.RecoveryPoint) *APIError {
	operation, failure := s.beginOperation(point.EnvironmentID)
	if failure != nil {
		return failure
	}
	defer operation.Close()
	envelope, ok := command.FromContext(ctx)
	if !ok {
		return diskAPIError("terminal", "missing_identity", "checkpoint abort requires identity")
	}
	host, failure := s.diskHost(ctx, point.HostID)
	if failure != nil {
		return failure
	}
	client, err := s.clientFactory(360 * time.Second)
	if err != nil {
		return internal(err)
	}
	point.State = "deleting"
	point.Phase = "aborting"
	if failure = s.saveRecoveryPoint(ctx, point); failure != nil {
		return failure
	}
	if err = s.advanceRecoveryFence(ctx, client, host, envelope, point.EnvironmentID, point.Generation, "abort-vm"); err != nil {
		return internal(err)
	}
	frozen, err := diskCall[model.FrozenVM](ctx, client, host, "GET", "/api/vms/"+point.VM.ID+"/checkpoint/freeze", nil)
	if err != nil {
		var typed *api.BoundaryError
		if !errors.As(err, &typed) || typed.Code != "not_found" {
			return internal(err)
		}
	} else {
		if frozen.PointID != point.ID {
			return diskAPIError("state", "checkpoint_busy", "another checkpoint owns the stopped VM")
		}
		if len(point.Disks) == 0 {
			point.VM = frozen.VM
			for _, volume := range frozen.Volumes {
				role := "data"
				if volume.ID == frozen.VM.Options.RootVolumeID {
					role = "root"
				}
				point.Disks = append(point.Disks, model.RecoveryDisk{VolumeID: volume.ID, SnapshotID: checkpointDiskID(point.ID, volume.ID, volume.Generation), Generation: volume.Generation, Role: role, Source: volume})
			}
			if failure = s.saveRecoveryPoint(ctx, point); failure != nil {
				return failure
			}
		}
	}
	for _, disk := range point.Disks {
		if err = s.advanceRecoveryFence(ctx, client, host, envelope, disk.VolumeID, disk.Generation, "abort-volume/"+disk.VolumeID); err != nil {
			return internal(err)
		}
		child := envelope
		child.Subject = disk.SnapshotID
		child.Generation = disk.Generation
		child.Key += "/abort-point/" + disk.SnapshotID
		if failure = s.deleteVolumeSnapshot(command.WithContext(ctx, child), disk.SnapshotID); failure != nil && !(failure.Failure != nil && failure.Failure.Code == "not_found") {
			return failure
		}
	}
	child := envelope
	child.Subject = point.EnvironmentID
	child.Generation = point.Generation
	child.Key += "/abort-thaw"
	response, err := client.request(command.WithContext(ctx, child), "POST", host.Addr, "/api/vms/"+point.VM.ID+"/checkpoint/thaw", api.CheckpointRequest{ID: point.ID}, nil)
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
		return diskAPIError("unknown", "checkpoint_incomplete", "checkpoint abort has not settled")
	}
	point.State = "deleted"
	point.Phase = "deleted"
	return s.saveRecoveryPoint(ctx, point)
}
