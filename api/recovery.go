package api

import "github.com/aisidev/tt/model"

type CheckpointRequest struct {
	ID string `json:"id"`
}
type RecoveryPointPage struct {
	Points      model.List[model.RecoveryPoint] `json:"points"`
	NextToken   string                          `json:"next_token,omitempty"`
	MaxPageSize int                             `json:"max_page_size"`
}
type RestorePointRequest struct {
	PointID     string            `json:"point_id"`
	Owner       string            `json:"owner"`
	CPU         *uint32           `json:"cpu,omitempty"`
	Mem         *uint32           `json:"mem,omitempty"`
	GuestConfig model.GuestConfig `json:"guest_config,omitempty"`
}
