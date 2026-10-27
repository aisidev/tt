package model

// InventoryItem reports evidence, never ownership authority. Unowned bytes are
// identified for quarantine without adoption or destruction.
type InventoryItem struct {
	Engine          Engine       `json:"engine,omitempty"`
	Image           string       `json:"image,omitempty"`
	CPU             uint32       `json:"cpu,omitempty"`
	MemMiB          uint32       `json:"mem_mib,omitempty"`
	RootVolumeID    string       `json:"root_volume_id,omitempty"`
	DataVolumeIDs   List[string] `json:"data_volume_ids,omitempty"`
	Unsettled       bool         `json:"unsettled,omitempty"`
	CapacityMiB     uint32       `json:"capacity_mib,omitempty"`
	ClaimMiB        uint32       `json:"claim_mib,omitempty"`
	CapacityUnknown bool         `json:"capacity_unknown,omitempty"`
	Subject         string       `json:"subject,omitempty"`
	Parent          string       `json:"parent,omitempty"`
	Kind            string       `json:"kind"`
	ID              string       `json:"id"`
	Metadata        bool         `json:"metadata"`
	Physical        bool         `json:"physical"`
	State           string       `json:"state,omitempty"`
	Generation      uint64       `json:"generation,omitempty"`
	Bytes           uint64       `json:"bytes"`
	Parts           List[string] `json:"parts"`
	Problem         string       `json:"problem,omitempty"`
	Evidence        string       `json:"evidence"`
}
type PhysicalInventory struct {
	QuarantineMiB    uint32              `json:"quarantine_mib"`
	QuarantinePinned bool                `json:"quarantine_pinned"`
	Processes        int                 `json:"processes"`
	ProcessDigest    string              `json:"process_digest"`
	ID               string              `json:"id"`
	HostID           string              `json:"host_id"`
	Authority        string              `json:"authority,omitempty"`
	Epoch            uint64              `json:"epoch"`
	Frozen           bool                `json:"frozen"`
	Complete         bool                `json:"complete"`
	Digest           string              `json:"digest"`
	Items            List[InventoryItem] `json:"items"`
	NextToken        string              `json:"next_token,omitempty"`
	MaxPageSize      int                 `json:"max_page_size"`
	Total            int                 `json:"total"`
}
