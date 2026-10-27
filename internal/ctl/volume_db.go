package ctl

import (
	"context"
	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/model"
)

func (d *DB) PutVolume(ctx context.Context, v *model.Volume) error {
	return putRecord(ctx, d.q, "INSERT OR REPLACE INTO volumes(id,data) VALUES(?1,?2)", "volume", v, v.ID)
}
func (d *DB) GetVolume(ctx context.Context, id string) (*model.Volume, error) {
	v, err := queryOne[model.Volume](ctx, d.q, "SELECT data FROM volumes WHERE id=?1", id)
	if err == nil {
		err = validVolumeCatalog(v, id)
	}
	return v, err
}
func (d *DB) ListVolumes(ctx context.Context) ([]model.Volume, error) {
	values, err := queryAll[model.Volume](ctx, d.q, "SELECT data FROM volumes ORDER BY id")
	if err != nil {
		return nil, err
	}
	for i := range values {
		if err = validVolumeCatalog(&values[i], values[i].ID); err != nil {
			return nil, err
		}
	}
	return values, nil
}
func (d *DB) PutVolumeSnapshot(ctx context.Context, v *model.VolumeSnapshot) error {
	return putRecord(ctx, d.q, "INSERT OR REPLACE INTO volume_snapshots(id,data) VALUES(?1,?2)", "volume checkpoint", v, v.ID)
}
func (d *DB) GetVolumeSnapshot(ctx context.Context, id string) (*model.VolumeSnapshot, error) {
	v, err := queryOne[model.VolumeSnapshot](ctx, d.q, "SELECT data FROM volume_snapshots WHERE id=?1", id)
	if err == nil {
		err = validCheckpointCatalog(v, id)
	}
	return v, err
}
func (d *DB) ListVolumeSnapshots(ctx context.Context) ([]model.VolumeSnapshot, error) {
	values, err := queryAll[model.VolumeSnapshot](ctx, d.q, "SELECT data FROM volume_snapshots ORDER BY id")
	if err != nil {
		return nil, err
	}
	for i := range values {
		if err = validCheckpointCatalog(&values[i], values[i].ID); err != nil {
			return nil, err
		}
	}
	return values, nil
}

func (d *DB) pendingDiskReservation(ctx context.Context, host string) (uint32, error) {
	volumes, err := d.ListVolumes(ctx)
	if err != nil {
		return 0, err
	}
	var total uint32
	for _, v := range volumes {
		if v.HostID == host && (v.State == "creating" || v.State == "growing") {
			total = model.SatAdd(total, v.ReservedMiB())
		}
	}
	points, err := d.ListVolumeSnapshots(ctx)
	if err != nil {
		return 0, err
	}
	for _, p := range points {
		if p.HostID == host && p.State == "creating" {
			total = model.SatAdd(total, p.Source.ReservedMiB())
		}
	}
	return total, nil
}

func validVolumeCatalog(v *model.Volume, id string) error {
	if v == nil {
		return nil
	}
	if v.ID == "" || v.ID != id || v.Generation == 0 || v.HostID == "" || v.SizeMiB == 0 || v.State == "" || v.Phase == "" {
		return &api.BoundaryError{Class: "terminal", Code: "malformed", Message: "malformed volume catalog"}
	}
	return nil
}
func validCheckpointCatalog(v *model.VolumeSnapshot, id string) error {
	if v == nil {
		return nil
	}
	if v.ID == "" || v.ID != id || v.Generation == 0 || v.HostID == "" || v.Source.ID == "" || v.State == "" || v.Phase == "" {
		return &api.BoundaryError{Class: "terminal", Code: "malformed", Message: "malformed checkpoint catalog"}
	}
	return nil
}
