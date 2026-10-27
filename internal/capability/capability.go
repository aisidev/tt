// Package capability defines design support and scoped host admission policy.
package capability

import (
	"fmt"
	"slices"
	"strings"

	"github.com/aisidev/tt/model"
)

const Version uint32 = 1

var Engines = []model.Engine{model.Qemu, model.Firecracker, model.Docker, model.Bhyve, model.Jail}
var Storage = []model.Storage{model.File, model.Zvol}

const (
	LegacySSH           = "ssh_bootstrap"
	LegacyConfig        = "guest_config"
	LegacyIsolation     = "isolated_network"
	LegacyBhyveEgress   = "bhyve_deny_outgoing"
	LegacyQemuResources = "qemu_resources"
	LegacyFCResources   = "firecracker_resources"
	LegacyFCDisk        = "firecracker_disk_resize"
	LegacyFCJailer      = "firecracker_jailer"
	LegacyFCZvol        = "firecracker_zvol"
	LegacyBackup        = "disk_backup_v1"
	LegacyBackupZvol    = "disk_backup_zvol_v1"
	LegacyBackupReflink = "disk_backup_reflink_v1"
)

var LegacyAll = []string{LegacySSH, LegacyConfig, LegacyIsolation, LegacyBhyveEgress, LegacyQemuResources, LegacyFCResources, LegacyFCDisk, LegacyFCJailer, LegacyFCZvol, LegacyBackup, LegacyBackupZvol, LegacyBackupReflink}

type Feature uint8

const (
	Run Feature = iota
	Ports
	SshBootstrap
	RootKeys
	GuestConfig
	DenyOutgoing
	Isolation
	DiskSize
	Resources
	Backup
)

var All = []Feature{Run, Ports, SshBootstrap, RootKeys, GuestConfig, DenyOutgoing, Isolation, DiskSize, Resources, Backup}
var tags = [...]string{"vm.run", "net.tcp.publish", "guest.ssh.bootstrap", "guest.ssh.root_keys", "guest.config.drive", "net.egress.deny", "net.isolation", "disk.size.create", "vm.resources.offline", "disk.backup.point"}
var meanings = [...]string{
	"Create and cold-start a workload with this engine and storage",
	"Publish TCP guest ports on the agent host",
	"Initialize an SSH account and report its access metadata",
	"Inject root authorized keys without managed SSH bootstrap",
	"Deliver caller-supplied opaque files on a read-only guest drive",
	"Block new routed guest egress while preserving incoming replies",
	"Enforce guest isolation from host, peers and private networks",
	"Request virtual root-disk capacity at creation",
	"Update CPU, memory and root-disk capacity while stopped",
	"Create a stopped root-disk recovery point using a qualified primitive",
}

func (f Feature) Tag() string     { return tags[f] }
func (f Feature) Meaning() string { return meanings[f] }
func (f Feature) Supported(e model.Engine, s model.Storage) bool {
	if e == model.Jail && s == model.Zvol {
		return false
	}
	switch f {
	case Run, Ports:
		return true
	case RootKeys:
		return e == model.Jail
	case GuestConfig:
		return e == model.Firecracker
	case DenyOutgoing:
		return e == model.Qemu || e == model.Firecracker || e == model.Bhyve
	case Backup:
		return e == model.Qemu || e == model.Firecracker || (e == model.Bhyve && s == model.Zvol)
	default:
		return e == model.Qemu || e == model.Firecracker
	}
}
func (f Feature) Limit(e model.Engine, s model.Storage) string {
	switch f {
	case SshBootstrap:
		return "Prepared guest image required; process state does not establish SSH readiness"
	case RootKeys:
		return "Root public keys only; no custom account, sudo or managed SSH metadata"
	case GuestConfig:
		return "Firecracker only; 32 files / 64 KiB, including the managed SSH seed"
	case DenyOutgoing:
		return "Not host or peer isolation; Jail and Docker do not implement this contract"
	case Isolation:
		return "Linux QEMU/Firecracker only; independent of outgoing restrictions"
	case DiskSize, Resources:
		if e == model.Qemu {
			return "No shrinking; guest partitions and filesystems must grow separately"
		}
		return "Stopped resource updates; Firecracker grows an unpartitioned ext4 root; no shrinking"
	case Backup:
		if s == model.File {
			return "Linux strict reflink qualification required; no full-copy fallback; existing backup recovery is separate from admission"
		}
		return "Stopped zvol only; verify ownership and capacity per operation; no memory or deleted-VM recovery"
	case Run:
		if e == model.Jail && s == model.Zvol {
			return "Jail requires file storage"
		}
		if e == model.Firecracker {
			return "Requires jailer and delegated cgroup controllers; zvol needs compatible agent support"
		}
		if e == model.Bhyve || e == model.Jail {
			return "Validated only on FreeBSD 15.x; manual host and guest setup required"
		}
		return "Process readiness is not application readiness"
	default:
		return "Reachability and reserved host port ranges remain deployment prerequisites"
	}
}
func (f Feature) RequireDesign(e model.Engine, s model.Storage) error {
	if f.Supported(e, s) {
		return nil
	}
	return fmt.Errorf("capability %s unsupported by design for %s/%s: %s", f.Tag(), e, s, f.Limit(e, s))
}

type Create struct{ Disk, Ports, Keys, SSH, GuestConfig, DenyOutgoing, Isolated bool }

func (c Create) Requirements(e model.Engine) []Feature {
	result := []Feature{Run}
	for _, item := range []struct {
		needed  bool
		feature Feature
	}{
		{c.Disk || e == model.Qemu, DiskSize},
		{c.Ports || e == model.Qemu || e == model.Bhyve || e == model.Jail || c.SSH || c.Keys, Ports},
		{c.SSH || (c.Keys && e != model.Jail), SshBootstrap},
		{c.Keys && e == model.Jail, RootKeys},
		{c.GuestConfig || ((c.SSH || c.Keys) && e == model.Firecracker), GuestConfig},
		{c.DenyOutgoing, DenyOutgoing}, {c.Isolated, Isolation},
	} {
		if item.needed {
			result = append(result, item.feature)
		}
	}
	return result
}
func Matrix() model.Matrix {
	m := model.Matrix{Version: Version, CreateFields: model.Map[string, model.List[string]]{
		"disk": {DiskSize.Tag()}, "ssh_keys": {SshBootstrap.Tag(), RootKeys.Tag()}, "guest_config": {GuestConfig.Tag()}, "deny_outgoing": {DenyOutgoing.Tag()}, "isolated_network": {Isolation.Tag()}, "ports": {Ports.Tag()},
	}}
	for _, f := range All {
		m.Definitions = append(m.Definitions, model.Definition{Tag: f.Tag(), Meaning: f.Meaning()})
	}
	for _, e := range Engines {
		for _, s := range Storage {
			platform := "linux"
			if e == model.Bhyve || e == model.Jail {
				platform = "freebsd"
			}
			entry := model.Combination{Engine: e, Storage: s, Platform: platform, Experimental: false}
			for _, f := range All {
				entry.Features = append(entry.Features, model.DesignSupport{Tag: f.Tag(), Supported: f.Supported(e, s), Limit: f.Limit(e, s)})
			}
			m.Combinations = append(m.Combinations, entry)
		}
	}
	return m
}
func missingLegacy(f Feature, e model.Engine, s model.Storage, tags []string) string {
	var required []string
	switch {
	case f == Run && e == model.Firecracker:
		required = []string{LegacyFCJailer}
		if s == model.Zvol {
			required = append(required, LegacyFCZvol)
		}
	case f == SshBootstrap:
		required = []string{LegacySSH}
	case f == GuestConfig:
		required = []string{LegacyConfig}
	case f == Isolation:
		required = []string{LegacyIsolation}
	case f == DenyOutgoing && e == model.Bhyve:
		required = []string{LegacyBhyveEgress}
	case f == DiskSize && e == model.Firecracker:
		required = []string{LegacyFCDisk}
	case f == Resources && e == model.Qemu:
		required = []string{LegacyQemuResources}
	case f == Resources && e == model.Firecracker:
		required = []string{LegacyFCResources}
	case f == Backup:
		required = []string{LegacyBackup}
	}
	for _, t := range required {
		if !slices.Contains(tags, t) {
			return t
		}
	}
	return ""
}
func prerequisiteReason(tag string) string {
	switch tag {
	case LegacySSH:
		return "ssh-keygen or ssh-keyscan is unavailable"
	case LegacyConfig:
		return "mkfs.ext4 is unavailable"
	case LegacyIsolation:
		return "nft or ip is unavailable"
	case LegacyFCJailer:
		return "delegated cgroup v2 cpu, memory or pids controllers are unavailable"
	case LegacyFCDisk, LegacyFCResources:
		return "e2fsck or resize2fs is unavailable"
	case LegacyFCZvol, LegacyQemuResources:
		return "engine or ZFS storage prerequisites are unavailable"
	case LegacyBhyveEgress:
		return "compatible bhyve outgoing-filter support was not advertised"
	case LegacyBackup:
		return "the disk backup v1 protocol was not advertised"
	default:
		return "a required host prerequisite was not advertised"
	}
}
func FromProbes(s model.Storage, engines []model.Engine, tags []string, admission bool, backupUnsupported *string) model.CapabilityReport {
	report := model.CapabilityReport{Version: Version, Storage: s, BackupAdmission: admission}
	for _, e := range Engines {
		entry := model.EngineReport{Engine: e}
		for _, f := range All {
			var reason *string
			switch {
			case !f.Supported(e, s):
				v := "unsupported by design: " + f.Limit(e, s)
				reason = &v
			case !slices.Contains(engines, e):
				v := "engine unavailable on this host"
				reason = &v
			default:
				if tag := missingLegacy(f, e, s, tags); tag != "" {
					v := fmt.Sprintf("%s (%s)", prerequisiteReason(tag), tag)
					reason = &v
				} else if f == Backup {
					reason = backupUnsupported
				}
			}
			status := model.CapabilityStatus{Tag: f.Tag(), Enabled: reason == nil, Reason: reason}
			if reason == nil {
				source := "implementation available; runtime and guest prerequisites checked during the operation"
				switch {
				case f == SshBootstrap:
					source = "tools: ssh-keygen and ssh-keyscan found"
				case f == GuestConfig:
					source = "tool: mkfs.ext4 probe succeeded"
				case f == Isolation:
					source = "tools: nft and ip probes succeeded"
				case (f == DiskSize || f == Resources) && e == model.Firecracker:
					source = "tools: e2fsck and resize2fs probes succeeded"
				case f == Backup && s == model.File:
					source = "storage: runtime filesystem passed strict reflink qualification"
				case f == Backup:
					source = "storage: ZFS tool probe succeeded; volume eligibility checked per request"
				}
				status.Evidence = model.List[string]{"engine detected: " + string(e), source}
			}
			entry.Features = append(entry.Features, status)
		}
		report.Engines = append(report.Engines, entry)
	}
	return report
}
func Require(report model.CapabilityReport, e model.Engine, s model.Storage, f Feature) error {
	if err := f.RequireDesign(e, s); err != nil {
		return err
	}
	denied := func(reason string) error {
		return fmt.Errorf("capability %s unavailable on host for %s/%s: %s", f.Tag(), e, s, reason)
	}
	if report.Version != Version {
		return denied("unknown capability report version; upgrade agent and controller")
	}
	if report.Storage != s {
		return denied("capability report storage does not match host")
	}
	var entry *model.EngineReport
	for i := range report.Engines {
		if report.Engines[i].Engine == e {
			if entry != nil {
				return denied("ambiguous engine report")
			}
			entry = &report.Engines[i]
		}
	}
	if entry == nil {
		return denied("engine report missing")
	}
	var status *model.CapabilityStatus
	for i := range entry.Features {
		if entry.Features[i].Tag == f.Tag() {
			if status != nil {
				return denied("ambiguous capability report")
			}
			status = &entry.Features[i]
		}
	}
	if status == nil {
		return denied("capability missing from report")
	}
	if !status.Enabled || status.Reason != nil {
		reason := "host did not enable this capability"
		if status.Reason != nil {
			reason = *status.Reason
		}
		return denied(reason)
	}
	return nil
}
func RequireHost(host model.Host, e model.Engine, features []Feature) error {
	if slices.Contains(features, Run) && !slices.Contains(host.Engines, e) {
		return fmt.Errorf("engine %s unavailable on host %s", e, host.ID)
	}
	for _, f := range features {
		if err := f.RequireDesign(e, host.Storage); err != nil {
			return err
		}
		if host.CapabilityReport != nil {
			if err := Require(*host.CapabilityReport, e, host.Storage, f); err != nil {
				return err
			}
		} else if tag := missingLegacy(f, e, host.Storage, host.Capabilities); tag != "" {
			return fmt.Errorf("capability %s unavailable on legacy host %s: missing %s; upgrade or configure the agent", f.Tag(), host.ID, tag)
		}
	}
	return nil
}
func BackupUnsupported(e model.Engine, s model.Storage, backendReason *string, report *model.CapabilityReport) *string {
	if err := Backup.RequireDesign(e, s); err != nil {
		v := "backup unsupported: " + err.Error()
		return &v
	}
	if backendReason != nil {
		return backendReason
	}
	if report != nil {
		if err := Require(*report, e, s, Backup); err != nil {
			v := "backup unsupported: " + err.Error()
			return &v
		}
	}
	return nil
}

// ValidateVMOptions is the policy part of Rust model::validate_vm_options.
// It lives here to keep the model package free of capability import cycles.
func ValidateVMOptions(e model.Engine, disk *uint32, deny bool, keys []string, ports []uint16) error {
	if disk != nil {
		if err := DiskSize.RequireDesign(e, model.File); err != nil {
			return err
		}
	}
	if deny {
		if err := DenyOutgoing.RequireDesign(e, model.File); err != nil {
			return err
		}
	}
	if len(keys) > 0 {
		f := SshBootstrap
		if e == model.Jail {
			f = RootKeys
		}
		if err := f.RequireDesign(e, model.File); err != nil {
			return err
		}
	}
	if len(ports) > 256 {
		return fmt.Errorf("at most 256 published ports are allowed per VM")
	}
	if slices.Contains(ports, 0) {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	for _, key := range keys {
		fields := strings.Fields(key)
		if strings.ContainsAny(key, "\n\r") || len(fields) < 2 || !(strings.HasPrefix(fields[0], "ssh-") || strings.HasPrefix(fields[0], "ecdsa-")) {
			return fmt.Errorf("invalid SSH public key; provide one OpenSSH public key per entry")
		}
	}
	return nil
}
