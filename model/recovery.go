package model

// FrozenVM captures the exact stopped disk set under a durable writer fence.
type FrozenVM struct {
	PointID string       `json:"point_id"`
	VM      Vm           `json:"vm"`
	Volumes List[Volume] `json:"volumes"`
}

type RecoveryDisk struct {
	VolumeID   string `json:"volume_id"`
	SnapshotID string `json:"snapshot_id"`
	Generation uint64 `json:"generation"`
	Role       string `json:"role"`
	Source     Volume `json:"source"`
}

// RecoveryPoint publishes one complete immutable disk set and its VM profile.
// A creating/deleting manifest is not a restorable point.
type RecoveryPoint struct {
	ID            string             `json:"id"`
	EnvironmentID string             `json:"environment_id"`
	Generation    uint64             `json:"generation"`
	Revision      uint64             `json:"revision"`
	HostID        string             `json:"host_id"`
	Owner         string             `json:"owner"`
	State         string             `json:"state"`
	Phase         string             `json:"phase"`
	VM            Vm                 `json:"vm"`
	Disks         List[RecoveryDisk] `json:"disks"`
	CreatedAt     uint64             `json:"created_at"`
	Error         *string            `json:"error,omitempty"`
}
