// Wire structures transcribed from the pinned Rust backup.rs.
package api

import "github.com/aisidev/tt/model"

type BackupView struct {
	Vm                model.Vm `json:"vm"`
	Enabled           bool     `json:"enabled"`
	Supported         bool     `json:"supported"`
	UnsupportedReason *string  `json:"unsupported_reason"`
}

func (v *BackupView) UnmarshalJSON(data []byte) error {
	type plain BackupView
	var next plain
	if err := model.DecodeRecord(data, &next, []string{"vm", "enabled", "supported"}, false, "unsupported_reason"); err != nil {
		return err
	}
	*v = BackupView(next)
	return nil
}

type RestoreBody struct {
	Generation string `json:"generation"`
}

func (v *RestoreBody) UnmarshalJSON(data []byte) error {
	type plain RestoreBody
	var next plain
	if err := model.DecodeRecord(data, &next, []string{"generation"}, true); err != nil {
		return err
	}
	*v = RestoreBody(next)
	return nil
}
