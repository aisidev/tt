// Wire structures transcribed from the pinned Rust capability.rs.
package model

type Definition struct {
	Tag     string `json:"tag"`
	Meaning string `json:"meaning"`
}

func (v *Definition) UnmarshalJSON(data []byte) error {
	type plain Definition
	var next plain
	if err := DecodeRecord(data, &next, []string{"tag", "meaning"}, false); err != nil {
		return err
	}
	*v = Definition(next)
	return nil
}

type DesignSupport struct {
	Tag       string `json:"tag"`
	Supported bool   `json:"supported"`
	Limit     string `json:"limit"`
}

func (v *DesignSupport) UnmarshalJSON(data []byte) error {
	type plain DesignSupport
	var next plain
	if err := DecodeRecord(data, &next, []string{"tag", "supported", "limit"}, false); err != nil {
		return err
	}
	*v = DesignSupport(next)
	return nil
}

type Combination struct {
	Engine       Engine              `json:"engine"`
	Storage      Storage             `json:"storage"`
	Platform     string              `json:"platform"`
	Experimental bool                `json:"experimental"`
	Features     List[DesignSupport] `json:"features"`
}

func (v *Combination) UnmarshalJSON(data []byte) error {
	type plain Combination
	var next plain
	if err := DecodeRecord(data, &next, []string{"engine", "storage", "platform", "experimental", "features"}, false); err != nil {
		return err
	}
	*v = Combination(next)
	return nil
}

type Matrix struct {
	Version      uint32                    `json:"version"`
	Definitions  List[Definition]          `json:"definitions"`
	CreateFields Map[string, List[string]] `json:"create_fields"`
	Combinations List[Combination]         `json:"combinations"`
}

func (v *Matrix) UnmarshalJSON(data []byte) error {
	type plain Matrix
	var next plain
	if err := DecodeRecord(data, &next, []string{"version", "definitions", "create_fields", "combinations"}, false); err != nil {
		return err
	}
	*v = Matrix(next)
	return nil
}

type CapabilityStatus struct {
	Tag      string       `json:"tag"`
	Enabled  bool         `json:"enabled"`
	Reason   *string      `json:"reason"`
	Evidence List[string] `json:"evidence,omitempty"`
}

func (v *CapabilityStatus) UnmarshalJSON(data []byte) error {
	type plain CapabilityStatus
	var next plain
	if err := DecodeRecord(data, &next, []string{"tag", "enabled"}, false, "reason"); err != nil {
		return err
	}
	*v = CapabilityStatus(next)
	return nil
}

type EngineReport struct {
	Engine   Engine                 `json:"engine"`
	Features List[CapabilityStatus] `json:"features"`
}

func (v *EngineReport) UnmarshalJSON(data []byte) error {
	type plain EngineReport
	var next plain
	if err := DecodeRecord(data, &next, []string{"engine", "features"}, false); err != nil {
		return err
	}
	*v = EngineReport(next)
	return nil
}

type CapabilityReport struct {
	Version         uint32             `json:"version"`
	Storage         Storage            `json:"storage"`
	Engines         List[EngineReport] `json:"engines"`
	BackupAdmission bool               `json:"backup_admission"`
}

func (v *CapabilityReport) UnmarshalJSON(data []byte) error {
	type plain CapabilityReport
	var next plain
	if err := DecodeRecord(data, &next, []string{"version", "storage", "engines", "backup_admission"}, false); err != nil {
		return err
	}
	*v = CapabilityReport(next)
	return nil
}
