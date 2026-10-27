package model

// Volume is a persistent disk independent of a workload process. All paths are
// derived from ID by the host; no caller supplies a filesystem path.
type Volume struct {
	ID              string  `json:"id"`
	HostID          string  `json:"host_id"`
	Owner           string  `json:"owner"`
	Generation      uint64  `json:"generation"`
	Revision        uint64  `json:"revision"`
	SizeMiB         uint32  `json:"size_mib"`
	Engine          Engine  `json:"engine,omitempty"`
	Image           string  `json:"image,omitempty"`
	Format          string  `json:"format"`
	Retain          bool    `json:"retain"`
	State           string  `json:"state"`
	AttachedTo      string  `json:"attached_to,omitempty"`
	AttachedEnv     string  `json:"attached_environment,omitempty"`
	Role            string  `json:"role,omitempty"`
	Phase           string  `json:"phase"`
	SourceSnapshot  string  `json:"source_snapshot,omitempty"`
	PendingSnapshot string  `json:"pending_snapshot,omitempty"`
	PendingGroup    string  `json:"pending_group,omitempty"`
	PendingSizeMiB  uint32  `json:"pending_size_mib,omitempty"`
	Error           *string `json:"error,omitempty"`
	CreatedAt       uint64  `json:"created_at"`
}

// VolumeSpec creates either a blank raw data disk or an image-initialized root.
// Snapshot sources preserve their engine/image/format and never mutate in place.
type VolumeSpec struct {
	ID           string `json:"id"`
	Owner        string `json:"owner,omitempty"`
	HostID       string `json:"host_id,omitempty"`
	Generation   uint64 `json:"generation"`
	SizeMiB      uint32 `json:"size_mib"`
	Engine       Engine `json:"engine,omitempty"`
	Image        string `json:"image,omitempty"`
	Retain       bool   `json:"retain"`
	FromSnapshot string `json:"from_snapshot,omitempty"`
}

type VolumeUpdate struct {
	SizeMiB *uint32 `json:"size_mib,omitempty"`
	Retain  *bool   `json:"retain,omitempty"`
}

type VolumeBinding struct {
	VMID string `json:"vm_id"`
	Role string `json:"role"`
}

// VolumeSnapshot is an immutable checkpoint. Metadata is retained until explicit
// deletion, including the source identity and the complete source disk shape.
type VolumeSnapshot struct {
	ID         string  `json:"id"`
	VolumeID   string  `json:"volume_id"`
	Owner      string  `json:"owner,omitempty"`
	HostID     string  `json:"host_id"`
	Generation uint64  `json:"generation"`
	Revision   uint64  `json:"revision"`
	State      string  `json:"state"`
	Phase      string  `json:"phase"`
	Source     Volume  `json:"source"`
	CreatedAt  uint64  `json:"created_at"`
	Error      *string `json:"error,omitempty"`
}

// DiskAttachment is host-derived launch configuration, persisted with the VM.
// A client cannot submit Path; the agent resolves it from its owned volume table.
type DiskAttachment struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	Format  string `json:"format"`
	SizeMiB uint32 `json:"size_mib"`
}

// ReservedMiB covers logical capacity plus bounded root-image metadata and kernel
// overhead. A raw data volume reserves precisely its declared capacity.
func (v Volume) ReservedMiB() uint32 {
	if v.State == "deleted" {
		return 0
	}
	if v.Image != "" {
		return SatAdd(v.SizeMiB, SatAdd(72, v.SizeMiB/128))
	}
	return v.SizeMiB
}
