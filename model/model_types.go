// Wire structures transcribed from the pinned Rust model.rs.
package model

type Resource struct {
	CPUTotal  uint32 `json:"cpu_total"`
	CPUUsed   uint32 `json:"cpu_used"`
	MemTotal  uint32 `json:"mem_total"`
	MemUsed   uint32 `json:"mem_used"`
	DiskTotal uint32 `json:"disk_total"`
	DiskUsed  uint32 `json:"disk_used"`
	VmCount   uint32 `json:"vm_count"`
}

func (v *Resource) UnmarshalJSON(data []byte) error {
	type plain Resource
	var next plain
	if err := DecodeRecord(data, &next, []string{"cpu_total", "cpu_used", "mem_total", "mem_used", "disk_total", "disk_used", "vm_count"}, false); err != nil {
		return err
	}
	*v = Resource(next)
	return nil
}

type Host struct {
	CapabilityReport *CapabilityReport   `json:"capability_report,omitempty"`
	Error            *string             `json:"error"`
	ImageSizes       Map[string, uint32] `json:"image_sizes"`
	Capabilities     List[string]        `json:"capabilities"`
	ID               string              `json:"id"`
	Addr             string              `json:"addr"`
	Resource         Resource            `json:"resource"`
	State            HostState           `json:"state"`
	Engines          List[Engine]        `json:"engines"`
	Images           List[string]        `json:"images"`
	Storage          Storage             `json:"storage"`
	RegisteredAt     uint64              `json:"registered_at"`
	StorageFeatures  *StorageFeatures    `json:"storage_features,omitempty"`
}

func (v *Host) UnmarshalJSON(data []byte) error {
	if err := CheckLegacySequence(data, 12); err != nil {
		return err
	}
	type plain Host
	var next plain
	if err := DecodeRecord(data, &next, []string{"id", "addr", "resource", "state", "engines", "storage", "registered_at"}, false); err != nil {
		return err
	}
	*v = Host(next)
	return nil
}

type Vm struct {
	Backup           BackupState         `json:"backup"`
	SSH              *SshInfo            `json:"ssh,omitempty"`
	PendingResources *VmResources        `json:"pending_resources,omitempty"`
	ID               string              `json:"id"`
	EnvID            string              `json:"env_id"`
	HostID           string              `json:"host_id"`
	Image            string              `json:"image"`
	Engine           Engine              `json:"engine"`
	CPU              uint32              `json:"cpu"`
	Mem              uint32              `json:"mem"`
	Disk             uint32              `json:"disk"`
	IP               string              `json:"ip"`
	PortMap          Map[uint16, uint16] `json:"port_map"`
	Options          VmOptions           `json:"options"`
	Error            *string             `json:"error"`
	State            VmState             `json:"state"`
	CreatedAt        uint64              `json:"created_at"`
}

func (v *Vm) UnmarshalJSON(data []byte) error {
	type plain Vm
	var next plain
	next.Backup = NewBackupState()
	if err := DecodeRecord(data, &next, []string{"id", "env_id", "host_id", "image", "engine", "cpu", "mem", "disk", "ip", "port_map", "state", "created_at"}, false); err != nil {
		return err
	}
	*v = Vm(next)
	return nil
}

type VmResources struct {
	CPU  uint32 `json:"cpu"`
	Mem  uint32 `json:"mem"`
	Disk uint32 `json:"disk"`
}

func (v *VmResources) UnmarshalJSON(data []byte) error {
	type plain VmResources
	var next plain
	if err := DecodeRecord(data, &next, []string{"cpu", "mem", "disk"}, true); err != nil {
		return err
	}
	*v = VmResources(next)
	return nil
}

type VmOptions struct {
	SSH               *SshOptions          `json:"ssh,omitempty"`
	Ports             List[uint16]         `json:"ports"`
	SSHKeys           List[string]         `json:"ssh_keys"`
	DenyOutgoing      bool                 `json:"deny_outgoing"`
	RequestedDisk     uint32               `json:"requested_disk"`
	IsolatedNetwork   bool                 `json:"isolated_network"`
	GuestConfigDigest *string              `json:"guest_config_digest,omitempty"`
	RootVolumeID      string               `json:"root_volume_id,omitempty"`
	DataVolumes       List[DiskAttachment] `json:"data_volumes,omitempty"`
	InitialStopped    bool                 `json:"initial_stopped,omitempty"`
	RootManaged       bool                 `json:"root_managed,omitempty"`
	RootDiskCharge    uint32               `json:"root_disk_charge,omitempty"`
	CreationComplete  bool                 `json:"creation_complete,omitempty"`
}

func (v *VmOptions) UnmarshalJSON(data []byte) error {
	if err := CheckLegacySequence(data, 7); err != nil {
		return err
	}
	type plain VmOptions
	var next plain
	if err := DecodeRecord(data, &next, []string{"ports", "ssh_keys", "deny_outgoing", "requested_disk"}, false); err != nil {
		return err
	}
	*v = VmOptions(next)
	return nil
}

type Env struct {
	ID           string       `json:"id"`
	Owner        string       `json:"owner"`
	VmIds        List[string] `json:"vm_ids"`
	CreatedAt    uint64       `json:"created_at"`
	ExpiresAt    uint64       `json:"expires_at"`
	Error        *string      `json:"error"`
	State        EnvState     `json:"state"`
	CreationHash string       `json:"creation_hash,omitempty"`
}

func (v *Env) UnmarshalJSON(data []byte) error {
	if err := CheckLegacySequence(data, 7); err != nil {
		return err
	}
	type plain Env
	var next plain
	if err := DecodeRecord(data, &next, []string{"id", "owner", "vm_ids", "created_at", "expires_at", "state"}, false); err != nil {
		return err
	}
	*v = Env(next)
	return nil
}
