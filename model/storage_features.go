package model

// StorageFeatures negotiates the persistent disk protocol independently of the
// build version. A missing report or unknown protocol is unsupported.
type StorageFeatures struct {
	ImageCatalog       bool `json:"image_catalog"`
	ProtocolVersion    int  `json:"protocol_version"`
	ArchiveAbort       bool `json:"archive_abort"`
	Archives           bool `json:"archives"`
	ImagePull          bool `json:"image_pull"`
	Volumes            bool `json:"volumes"`
	Snapshots          bool `json:"snapshots"`
	WorkspaceSnapshots bool `json:"workspace_snapshots"`
	DataDisks          bool `json:"data_disks"`
}

func (f *StorageFeatures) SupportsVolumes() bool {
	return f != nil && f.ProtocolVersion == 1 && f.Volumes
}
