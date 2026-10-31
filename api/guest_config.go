package api

import "github.com/aisidev/tt/model"

// GuestConfigStart is the optional, explicitly negotiated Firecracker reseed
// protocol. Contents are private bootstrap data, never response fields.
type GuestConfigStart struct {
	Version uint32            `json:"version"`
	Files   model.GuestConfig `json:"files"`
}

type GuestConfigCapabilities struct {
	ProtocolVersion uint32 `json:"protocol_version"`
	StartWithConfig bool   `json:"start_with_config"`
}
