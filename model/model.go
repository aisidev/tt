// Package model defines TTstack's persistent entities and shared wire values.
// It deliberately has no dependencies on other internal packages.
package model

import (
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"
)

const (
	VMCPUDefault  uint32 = 2
	VMMemDefault  uint32 = 1024
	VMDiskDefault uint32 = 40 * 1024
	// DefaultLifetime is permanent unless a caller explicitly sets an expiry.
	DefaultLifetime uint64 = 0
	MaxHosts               = 50
	MaxVMs                 = 1000
	RunDir                 = "/home/ttstack/run"
	ConfigDiskMiB   uint32 = 4
)

type GuestConfig = Map[string, string]

func SatAdd(a, b uint32) uint32 {
	if b > math.MaxUint32-a {
		return math.MaxUint32
	}
	return a + b
}
func SatSub(a, b uint32) uint32 {
	if b > a {
		return 0
	}
	return a - b
}
func (r Resource) CPUFree() uint32  { return SatSub(r.CPUTotal, r.CPUUsed) }
func (r Resource) MemFree() uint32  { return SatSub(r.MemTotal, r.MemUsed) }
func (r Resource) DiskFree() uint32 { return SatSub(r.DiskTotal, r.DiskUsed) }
func (r Resource) CanFit(cpu, mem, disk uint32) bool {
	return r.CPUFree() >= cpu && r.MemFree() >= mem && r.DiskFree() >= disk
}
func (e Engine) MemoryReservation(mem uint32) uint32 {
	if e == Firecracker {
		return SatAdd(mem, 128)
	}
	return mem
}
func (e Engine) DefaultDisk() uint32 {
	if e == Qemu {
		return VMDiskDefault
	}
	return 0
}
func (o VmOptions) ConfigDiskMiB(engine Engine) uint32 {
	if engine == Firecracker && (o.GuestConfigDigest != nil || o.SSH != nil) {
		return ConfigDiskMiB
	}
	return 0
}
func (v Vm) ReservedDisk() uint32 {
	disk := v.Disk
	if v.Options.RootManaged {
		disk = SatAdd(v.Options.RootDiskCharge, v.Options.ConfigDiskMiB(v.Engine))
	}
	if v.PendingResources != nil {
		disk = max(disk, SatAdd(v.PendingResources.Disk, v.Options.ConfigDiskMiB(v.Engine)))
	}
	return SatAdd(disk, v.Backup.ReservedMiB())
}
func (r *Resource) Account(v Vm) {
	r.DiskUsed = SatAdd(r.DiskUsed, v.ReservedDisk())
	r.VmCount = SatAdd(r.VmCount, 1)
	if v.State != VmStopped && !(v.Options.InitialStopped && !v.Options.CreationComplete) {
		r.CPUUsed = SatAdd(r.CPUUsed, v.CPU)
		r.MemUsed = SatAdd(r.MemUsed, v.Engine.MemoryReservation(v.Mem))
	}
}

// Release undoes Account; callers use it when a VM leaves the fleet.
func (r *Resource) Release(v Vm) {
	r.DiskUsed = SatSub(r.DiskUsed, v.ReservedDisk())
	r.VmCount = SatSub(r.VmCount, 1)
	if v.State != VmStopped && !(v.Options.InitialStopped && !v.Options.CreationComplete) {
		r.CPUUsed = SatSub(r.CPUUsed, v.CPU)
		r.MemUsed = SatSub(r.MemUsed, v.Engine.MemoryReservation(v.Mem))
	}
}
func NewBackupState() BackupState { return BackupState{Revision: uuid.NewString()} }
func (g BackupGeneration) ReservedMiB() uint32 {
	n := g.RootBytes / (1024 * 1024)
	if g.RootBytes%(1024*1024) != 0 {
		n++
	}
	if n > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(n)
}
func (s BackupState) ReservedMiB() uint32 {
	var committed, pending uint32
	if s.Current != nil {
		committed = s.Current.ReservedMiB()
	}
	for _, g := range s.Retired {
		committed = SatAdd(committed, g.ReservedMiB())
	}
	if s.Pending != nil && s.Pending.Request.Action != BackupDelete {
		pending = s.Pending.Artifact.ReservedMiB()
	}
	return SatAdd(committed, max(pending, s.ForwardingReservedMiB))
}
func (s BackupState) Busy() bool         { return s.Pending != nil || s.Forwarding != nil }
func (s BackupState) HasArtifacts() bool { return s.Current != nil || len(s.Retired) > 0 || s.Busy() }
func DefaultSshOptions() SshOptions      { return SshOptions{User: "root"} }
func (o SshOptions) Validate() error {
	valid := len(o.User) > 0 && len(o.User) <= 32
	if valid {
		valid = o.User[0] >= 'a' && o.User[0] <= 'z'
	}
	for _, c := range []byte(o.User) {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			valid = false
		}
	}
	if !valid {
		return fmt.Errorf("ssh.user must be a lowercase Unix account name")
	}
	return nil
}
func ASCIIAlphanumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
func ValidateName(name, label string) error {
	if len(name) == 0 {
		return fmt.Errorf("%s cannot be empty", label)
	}
	if len(name) > 128 {
		return fmt.Errorf("%s too long (max 128 chars)", label)
	}
	for _, c := range []byte(name) {
		if !ASCIIAlphanumeric(c) && !strings.ContainsRune("-_.", rune(c)) {
			return fmt.Errorf("%s contains invalid characters (only a-z, A-Z, 0-9, '-', '_', '.' allowed)", label)
		}
	}
	if strings.HasPrefix(name, ".") || strings.Contains(name, "..") {
		return fmt.Errorf("%s must not start with '.' or contain '..'", label)
	}
	return nil
}
func ValidateImage(image string, engine Engine) error {
	if engine != Docker {
		return ValidateName(image, "image")
	}
	valid := len(image) > 0 && len(image) <= 512
	if valid {
		valid = ASCIIAlphanumeric(image[0])
	}
	for _, c := range []byte(image) {
		if !ASCIIAlphanumeric(c) && !strings.ContainsRune("._-/:@", rune(c)) {
			valid = false
		}
	}
	if !valid {
		return fmt.Errorf("invalid container image reference")
	}
	return nil
}
