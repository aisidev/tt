package model

// ControlReport negotiates receiver-wide epoch fencing separately from builds.
// An enabled receiver rejects every command outside its external authority epoch.
type ControlReport struct {
	EnrollmentVersion int                     `json:"enrollment_version,omitempty"`
	ActivationVersion int                     `json:"activation_version,omitempty"`
	Activations       List[ActivationReceipt] `json:"activations,omitempty"`
	ScopeDigest       string                  `json:"scope_digest,omitempty"`
	Engines           List[Engine]            `json:"engines,omitempty"`
	Fence             *FencingReceipt         `json:"fence,omitempty"`
	ProtocolVersion   int                     `json:"protocol_version"`
	Enabled           bool                    `json:"enabled"`
	Authority         string                  `json:"authority,omitempty"`
	Epoch             uint64                  `json:"epoch"`
	Phase             string                  `json:"phase"`
}
type EnrollmentRequest struct {
	HostID      string `json:"host_id"`
	Endpoint    string `json:"endpoint"`
	ScopeDigest string `json:"scope_digest"`
	PlanDigest  string `json:"plan_digest"`
}
type EnrollmentReceipt struct {
	Version   int               `json:"version"`
	ID        string            `json:"id"`
	Authority string            `json:"authority"`
	Epoch     uint64            `json:"epoch"`
	Key       string            `json:"key"`
	Request   EnrollmentRequest `json:"request"`
}

// ActivationRequest binds a phase transition to this receiver's own fence and
// complete frozen inventory. Catalog and plan digests are the caller's durable
// composition evidence; neither is a caller-provided physical fencing claim.
type ActivationRequest struct {
	Phase           string `json:"phase"`
	FenceID         string `json:"fence_id"`
	Previous        string `json:"previous"`
	InventoryDigest string `json:"inventory_digest,omitempty"`
	CatalogDigest   string `json:"catalog_digest"`
	PlanDigest      string `json:"plan_digest"`
}
type ActivationReceipt struct {
	QuarantineMiB    uint32            `json:"quarantine_mib"`
	QuarantinePinned bool              `json:"quarantine_pinned"`
	Version          int               `json:"version"`
	ID               string            `json:"id"`
	Authority        string            `json:"authority"`
	Epoch            uint64            `json:"epoch"`
	DecisionKey      string            `json:"decision_key"`
	HostID           string            `json:"host_id"`
	Key              string            `json:"key"`
	Method           string            `json:"method"`
	ScopeDigest      string            `json:"scope_digest"`
	Request          ActivationRequest `json:"request"`
	CompletedAt      string            `json:"completed_at"`
}

type FencingReceipt struct {
	Engines       List[Engine] `json:"engines"`
	Version       int          `json:"version"`
	ID            string       `json:"id"`
	Authority     string       `json:"authority"`
	Epoch         uint64       `json:"epoch"`
	HostID        string       `json:"host_id"`
	Key           string       `json:"key"`
	Method        string       `json:"method"`
	ScopeDigest   string       `json:"scope_digest"`
	ProcessDigest string       `json:"process_digest"`
	KnownVMs      int          `json:"known_vms"`
	CompletedAt   string       `json:"completed_at"`
}
