// Wire structures transcribed from the pinned Rust api.rs.
package api

import "github.com/aisidev/tt/model"

type CreateVmReq struct {
	VmID            string             `json:"vm_id"`
	EnvID           string             `json:"env_id"`
	Image           string             `json:"image"`
	Engine          model.Engine       `json:"engine"`
	CPU             uint32             `json:"cpu"`
	Mem             uint32             `json:"mem"`
	Disk            uint32             `json:"disk"`
	Ports           model.List[uint16] `json:"ports"`
	DenyOutgoing    bool               `json:"deny_outgoing"`
	IsolatedNetwork bool               `json:"isolated_network"`
	GuestConfig     model.GuestConfig  `json:"guest_config,omitempty"`
	SSHKeys         model.List[string] `json:"ssh_keys,omitempty"`
	SSH             *model.SshOptions  `json:"ssh,omitempty"`
	RootVolumeID    string             `json:"root_volume_id,omitempty"`
	DataVolumeIDs   model.List[string] `json:"data_volume_ids,omitempty"`
	DurableRoot     bool               `json:"durable_root,omitempty"`
	RootRetain      *bool              `json:"root_retain,omitempty"`
	HostID          string             `json:"host_id,omitempty"`
	InitialStopped  bool               `json:"initial_stopped,omitempty"`
	VolumeOwner     string             `json:"volume_owner,omitempty"`
}

func (v *CreateVmReq) UnmarshalJSON(data []byte) error {
	if err := model.CheckLegacySequence(data, 13); err != nil {
		return err
	}
	type plain CreateVmReq
	var next plain
	if err := model.DecodeRecord(data, &next, []string{"vm_id", "env_id", "image", "engine", "cpu", "mem", "disk", "ports", "deny_outgoing"}, false); err != nil {
		return err
	}
	*v = CreateVmReq(next)
	return nil
}

type CreateVmResp struct {
	Vm model.Vm `json:"vm"`
}

func (v *CreateVmResp) UnmarshalJSON(data []byte) error {
	type plain CreateVmResp
	var next plain
	if err := model.DecodeRecord(data, &next, []string{"vm"}, false); err != nil {
		return err
	}
	*v = CreateVmResp(next)
	return nil
}

type AgentInfo struct {
	CapabilityReport *model.CapabilityReport   `json:"capability_report,omitempty"`
	VMs              *model.List[model.Vm]     `json:"vms,omitempty"`
	Warnings         model.List[string]        `json:"warnings,omitempty"`
	ImageSizes       model.Map[string, uint32] `json:"image_sizes"`
	Capabilities     model.List[string]        `json:"capabilities"`
	HostID           string                    `json:"host_id"`
	Resource         model.Resource            `json:"resource"`
	Engines          model.List[model.Engine]  `json:"engines"`
	Storage          model.Storage             `json:"storage"`
	Images           model.List[string]        `json:"images"`
	StorageFeatures  *model.StorageFeatures    `json:"storage_features,omitempty"`
}

func (v *AgentInfo) UnmarshalJSON(data []byte) error {
	if err := model.CheckLegacySequence(data, 10); err != nil {
		return err
	}
	type plain AgentInfo
	var next plain
	if err := model.DecodeRecord(data, &next, []string{"host_id", "resource", "engines", "storage", "images"}, false); err != nil {
		return err
	}
	*v = AgentInfo(next)
	return nil
}

type VmSpec struct {
	Image           string             `json:"image"`
	Engine          model.Engine       `json:"engine"`
	CPU             *uint32            `json:"cpu"`
	Mem             *uint32            `json:"mem"`
	Disk            *uint32            `json:"disk"`
	Ports           model.List[uint16] `json:"ports"`
	DenyOutgoing    bool               `json:"deny_outgoing"`
	IsolatedNetwork bool               `json:"isolated_network"`
	GuestConfig     model.GuestConfig  `json:"guest_config,omitempty"`
	SSHKeys         model.List[string] `json:"ssh_keys,omitempty"`
	SSH             *model.SshOptions  `json:"ssh,omitempty"`
	RootVolumeID    string             `json:"root_volume_id,omitempty"`
	DataVolumeIDs   model.List[string] `json:"data_volume_ids,omitempty"`
	DurableRoot     bool               `json:"durable_root,omitempty"`
	RootRetain      *bool              `json:"root_retain,omitempty"`
	HostID          string             `json:"host_id,omitempty"`
	InitialStopped  bool               `json:"initial_stopped,omitempty"`
}

func (v *VmSpec) UnmarshalJSON(data []byte) error {
	if err := model.CheckLegacySequence(data, 11); err != nil {
		return err
	}
	type plain VmSpec
	var next plain
	next.Engine = model.Qemu
	if err := model.DecodeRecord(data, &next, []string{"image"}, false, "cpu", "mem", "disk"); err != nil {
		return err
	}
	*v = VmSpec(next)
	return nil
}

type CreateEnvReq struct {
	ID       string             `json:"id"`
	Owner    string             `json:"owner"`
	VMs      model.List[VmSpec] `json:"vms"`
	Lifetime *uint64            `json:"lifetime"`
	SSHKeys  model.List[string] `json:"ssh_keys,omitempty"`
}

func (v *CreateEnvReq) UnmarshalJSON(data []byte) error {
	type plain CreateEnvReq
	var next plain
	if err := model.DecodeRecord(data, &next, []string{"id", "vms"}, false, "lifetime"); err != nil {
		return err
	}
	*v = CreateEnvReq(next)
	return nil
}

type EnvDetail struct {
	Env                 model.Env            `json:"env"`
	VMs                 model.List[model.Vm] `json:"vms"`
	Warnings            model.List[string]   `json:"warnings,omitempty"`
	ObservationRevision uint64               `json:"observation_revision,omitempty"`
}

func (v *EnvDetail) UnmarshalJSON(data []byte) error {
	if err := model.CheckLegacySequence(data, 3); err != nil {
		return err
	}
	type plain EnvDetail
	var next plain
	if err := model.DecodeRecord(data, &next, []string{"env", "vms"}, false); err != nil {
		return err
	}
	*v = EnvDetail(next)
	return nil
}

type RegisterHostReq struct {
	Addr string `json:"addr"`
}

func (v *RegisterHostReq) UnmarshalJSON(data []byte) error {
	type plain RegisterHostReq
	var next plain
	if err := model.DecodeRecord(data, &next, []string{"addr"}, false); err != nil {
		return err
	}
	*v = RegisterHostReq(next)
	return nil
}

type ImageInfo struct {
	Name   string `json:"name"`
	HostID string `json:"host_id"`
}

func (v *ImageInfo) UnmarshalJSON(data []byte) error {
	type plain ImageInfo
	var next plain
	if err := model.DecodeRecord(data, &next, []string{"name", "host_id"}, false); err != nil {
		return err
	}
	*v = ImageInfo(next)
	return nil
}

type FleetStatus struct {
	Hosts       uint32 `json:"hosts"`
	HostsOnline uint32 `json:"hosts_online"`
	TotalVMs    uint32 `json:"total_vms"`
	TotalEnvs   uint32 `json:"total_envs"`
	CPUTotal    uint32 `json:"cpu_total"`
	CPUUsed     uint32 `json:"cpu_used"`
	MemTotal    uint32 `json:"mem_total"`
	MemUsed     uint32 `json:"mem_used"`
	DiskTotal   uint32 `json:"disk_total"`
	DiskUsed    uint32 `json:"disk_used"`
}

func (v *FleetStatus) UnmarshalJSON(data []byte) error {
	type plain FleetStatus
	var next plain
	if err := model.DecodeRecord(data, &next, []string{"hosts", "hosts_online", "total_vms", "total_envs", "cpu_total", "cpu_used", "mem_total", "mem_used", "disk_total", "disk_used"}, false); err != nil {
		return err
	}
	*v = FleetStatus(next)
	return nil
}
