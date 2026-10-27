package api

import "github.com/aisidev/tt/model"

type ArchiveImportRequest struct {
	Pull model.ArchivePull `json:"pull"`
	Spec model.VolumeSpec  `json:"spec"`
}
