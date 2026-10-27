package model

import (
	"encoding/json"
	"fmt"
)

type Engine string

const (
	Qemu        Engine = "qemu"
	Firecracker Engine = "firecracker"
	Bhyve       Engine = "bhyve"
	Docker      Engine = "docker"
	Jail        Engine = "jail"
)

func (v Engine) String() string { return string(v) }
func (v Engine) Valid() bool {
	switch v {
	case Qemu, Firecracker, Bhyve, Docker, Jail:
		return true
	}
	return false
}
func (v Engine) MarshalJSON() ([]byte, error) {
	if !v.Valid() {
		return nil, fmt.Errorf("unknown Engine: %s", v)
	}
	return json.Marshal(string(v))
}
func (v *Engine) UnmarshalJSON(data []byte) error {
	s, err := decodeUnitVariant(data)
	if err != nil {
		return err
	}
	next := Engine(s)
	if !next.Valid() {
		return fmt.Errorf("unknown Engine: %s", s)
	}
	*v = next
	return nil
}

type Storage string

const (
	File Storage = "file"
	Zvol Storage = "zvol"
)

func (v Storage) String() string { return string(v) }
func (v Storage) Valid() bool {
	switch v {
	case File, Zvol:
		return true
	}
	return false
}
func (v Storage) MarshalJSON() ([]byte, error) {
	if !v.Valid() {
		return nil, fmt.Errorf("unknown Storage: %s", v)
	}
	return json.Marshal(string(v))
}
func (v *Storage) UnmarshalJSON(data []byte) error {
	s, err := decodeUnitVariant(data)
	if err != nil {
		return err
	}
	next := Storage(s)
	if !next.Valid() {
		return fmt.Errorf("unknown Storage: %s", s)
	}
	*v = next
	return nil
}

type VmState string

const (
	VmRunning  VmState = "running"
	VmStopped  VmState = "stopped"
	VmPaused   VmState = "paused"
	VmCreating VmState = "creating"
	VmFailed   VmState = "failed"
	VmDeleting VmState = "deleting"
)

func (v VmState) String() string { return string(v) }
func (v VmState) Valid() bool {
	switch v {
	case VmRunning, VmStopped, VmPaused, VmCreating, VmFailed, VmDeleting:
		return true
	}
	return false
}
func (v VmState) MarshalJSON() ([]byte, error) {
	if !v.Valid() {
		return nil, fmt.Errorf("unknown VmState: %s", v)
	}
	return json.Marshal(string(v))
}
func (v *VmState) UnmarshalJSON(data []byte) error {
	s, err := decodeUnitVariant(data)
	if err != nil {
		return err
	}
	next := VmState(s)
	if !next.Valid() {
		return fmt.Errorf("unknown VmState: %s", s)
	}
	*v = next
	return nil
}

type EnvState string

const (
	EnvActive   EnvState = "active"
	EnvStopped  EnvState = "stopped"
	EnvCreating EnvState = "creating"
	EnvDeleting EnvState = "deleting"
	EnvFailed   EnvState = "failed"
)

func (v EnvState) String() string { return string(v) }
func (v EnvState) Valid() bool {
	switch v {
	case EnvActive, EnvStopped, EnvCreating, EnvDeleting, EnvFailed:
		return true
	}
	return false
}
func (v EnvState) MarshalJSON() ([]byte, error) {
	if !v.Valid() {
		return nil, fmt.Errorf("unknown EnvState: %s", v)
	}
	return json.Marshal(string(v))
}
func (v *EnvState) UnmarshalJSON(data []byte) error {
	s, err := decodeUnitVariant(data)
	if err != nil {
		return err
	}
	next := EnvState(s)
	if !next.Valid() {
		return fmt.Errorf("unknown EnvState: %s", s)
	}
	*v = next
	return nil
}

type HostState string

const (
	HostOnline  HostState = "online"
	HostOffline HostState = "offline"
)

func (v HostState) String() string { return string(v) }
func (v HostState) Valid() bool {
	switch v {
	case HostOnline, HostOffline:
		return true
	}
	return false
}
func (v HostState) MarshalJSON() ([]byte, error) {
	if !v.Valid() {
		return nil, fmt.Errorf("unknown HostState: %s", v)
	}
	return json.Marshal(string(v))
}
func (v *HostState) UnmarshalJSON(data []byte) error {
	s, err := decodeUnitVariant(data)
	if err != nil {
		return err
	}
	next := HostState(s)
	if !next.Valid() {
		return fmt.Errorf("unknown HostState: %s", s)
	}
	*v = next
	return nil
}

type BackupBackend string

const (
	BackupZvol    BackupBackend = "zvol"
	BackupReflink BackupBackend = "reflink"
)

func (v BackupBackend) String() string { return string(v) }
func (v BackupBackend) Valid() bool {
	switch v {
	case BackupZvol, BackupReflink:
		return true
	}
	return false
}
func (v BackupBackend) MarshalJSON() ([]byte, error) {
	if !v.Valid() {
		return nil, fmt.Errorf("unknown BackupBackend: %s", v)
	}
	return json.Marshal(string(v))
}
func (v *BackupBackend) UnmarshalJSON(data []byte) error {
	s, err := decodeUnitVariant(data)
	if err != nil {
		return err
	}
	next := BackupBackend(s)
	if !next.Valid() {
		return fmt.Errorf("unknown BackupBackend: %s", s)
	}
	*v = next
	return nil
}

type BackupAction string

const (
	BackupCreate  BackupAction = "create"
	BackupRestore BackupAction = "restore"
	BackupDelete  BackupAction = "delete"
)

func (v BackupAction) String() string { return string(v) }
func (v BackupAction) Valid() bool {
	switch v {
	case BackupCreate, BackupRestore, BackupDelete:
		return true
	}
	return false
}
func (v BackupAction) MarshalJSON() ([]byte, error) {
	if !v.Valid() {
		return nil, fmt.Errorf("unknown BackupAction: %s", v)
	}
	return json.Marshal(string(v))
}
func (v *BackupAction) UnmarshalJSON(data []byte) error {
	s, err := decodeUnitVariant(data)
	if err != nil {
		return err
	}
	next := BackupAction(s)
	if !next.Valid() {
		return fmt.Errorf("unknown BackupAction: %s", s)
	}
	*v = next
	return nil
}

func ParseEngine(value string) (Engine, error) {
	switch asciiLower(value) {
	case "qemu", "kvm":
		return Qemu, nil
	case "firecracker", "fc":
		return Firecracker, nil
	case "bhyve":
		return Bhyve, nil
	case "docker", "podman":
		return Docker, nil
	case "jail":
		return Jail, nil
	default:
		return "", fmt.Errorf("unknown engine: %s", value)
	}
}
func ParseStorage(value string) (Storage, error) {
	s := Storage(asciiLower(value))
	if !s.Valid() {
		return "", fmt.Errorf("unknown storage backend: %s", value)
	}
	return s, nil
}

func asciiLower(value string) string {
	bytes := []byte(value)
	for i, c := range bytes {
		if c >= 'A' && c <= 'Z' {
			bytes[i] = c + ('a' - 'A')
		}
	}
	return string(bytes)
}
