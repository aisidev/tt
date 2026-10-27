package api

import "github.com/aisidev/tt/model"

type VolumePage struct {
	Volumes     model.List[model.Volume] `json:"volumes"`
	NextToken   string                   `json:"next_token,omitempty"`
	MaxPageSize int                      `json:"max_page_size"`
}
type VolumeSnapshotPage struct {
	Snapshots   model.List[model.VolumeSnapshot] `json:"snapshots"`
	NextToken   string                           `json:"next_token,omitempty"`
	MaxPageSize int                              `json:"max_page_size"`
}
type SnapshotVolumeRequest struct {
	ID string `json:"id"`
}
