// Wire structures transcribed from the pinned Rust ssh.rs.
package model

type SshOptions struct {
	User string `json:"user"`
	Sudo bool   `json:"sudo"`
}

func (v *SshOptions) UnmarshalJSON(data []byte) error {
	type plain SshOptions
	var next plain
	if err := DecodeRecord(data, &next, []string{"user"}, true); err != nil {
		return err
	}
	*v = SshOptions(next)
	return nil
}

type SshInfo struct {
	ObservationError *string `json:"observation_error,omitempty"`
	User             string  `json:"user"`
	Sudo             bool    `json:"sudo"`
	Host             string  `json:"host"`
	Port             uint16  `json:"port"`
	HostKey          string  `json:"host_key"`
	Ready            bool    `json:"ready"`
	CheckedAt        uint64  `json:"checked_at"`
	Initialized      bool    `json:"initialized"`
}

func (v *SshInfo) UnmarshalJSON(data []byte) error {
	type plain SshInfo
	var next plain
	if err := DecodeRecord(data, &next, []string{"user", "sudo", "host", "port", "host_key"}, false); err != nil {
		return err
	}
	*v = SshInfo(next)
	return nil
}
