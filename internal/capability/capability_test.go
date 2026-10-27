package capability

import (
	"slices"
	"strings"
	"testing"

	"github.com/aisidev/tt/model"
)

func TestMatrixPreservesDistinctGuestAndNetworkContracts(t *testing.T) {
	m := Matrix()
	if len(m.Combinations) != 10 || len(m.Definitions) != len(All) {
		t.Fatal(m)
	}
	for _, entry := range m.Combinations {
		platform := "linux"
		if entry.Engine == model.Bhyve || entry.Engine == model.Jail {
			platform = "freebsd"
		}
		if len(entry.Features) != len(All) || entry.Experimental || entry.Platform != platform {
			t.Fatal(entry)
		}
		if entry.Engine == model.Bhyve || entry.Engine == model.Jail {
			if got := Run.Limit(entry.Engine, entry.Storage); entry.Engine == model.Jail && entry.Storage == model.Zvol {
				if got != "Jail requires file storage" {
					t.Fatal(got)
				}
			} else if got != "Validated only on FreeBSD 15.x; manual host and guest setup required" {
				t.Fatal(got)
			}
		}
	}
	for _, c := range []struct {
		f    Feature
		e    model.Engine
		s    model.Storage
		want bool
	}{{GuestConfig, model.Qemu, model.File, false}, {DenyOutgoing, model.Bhyve, model.Zvol, true}, {Isolation, model.Bhyve, model.Zvol, false}, {RootKeys, model.Jail, model.File, true}, {SshBootstrap, model.Jail, model.File, false}, {Backup, model.Bhyve, model.File, false}, {Backup, model.Bhyve, model.Zvol, true}} {
		if c.f.Supported(c.e, c.s) != c.want {
			t.Fatal(c)
		}
	}
	for _, f := range All {
		if f.Supported(model.Jail, model.Zvol) {
			t.Fatal(f)
		}
	}
}
func TestRequirementsDistinguishRootKeysBootstrapEgressAndIsolation(t *testing.T) {
	fc := (Create{Keys: true}).Requirements(model.Firecracker)
	for _, f := range []Feature{SshBootstrap, GuestConfig, Ports} {
		if !slices.Contains(fc, f) {
			t.Fatal(fc)
		}
	}
	jail := (Create{Keys: true}).Requirements(model.Jail)
	if !slices.Contains(jail, RootKeys) || slices.Contains(jail, SshBootstrap) {
		t.Fatal(jail)
	}
}
func TestReportFailsClosedOnVersionScopeAmbiguityAndReason(t *testing.T) {
	report := FromProbes(model.File, []model.Engine{model.Qemu}, LegacyAll, true, nil)
	if err := Require(report, model.Qemu, model.File, Run); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*model.CapabilityReport){
		func(r *model.CapabilityReport) { r.Version++ },
		func(r *model.CapabilityReport) { r.Storage = model.Zvol },
		func(r *model.CapabilityReport) { r.Engines = nil },
		func(r *model.CapabilityReport) { r.Engines = append(r.Engines, r.Engines[0]) },
		func(r *model.CapabilityReport) {
			r.Engines[0].Features = append(r.Engines[0].Features, r.Engines[0].Features[0])
		},
		func(r *model.CapabilityReport) { reason := "blocked"; r.Engines[0].Features[0].Reason = &reason },
	} {
		r := FromProbes(model.File, []model.Engine{model.Qemu}, LegacyAll, true, nil)
		mutate(&r)
		if Require(r, model.Qemu, model.File, Run) == nil {
			t.Fatal("invalid report admitted")
		}
	}
}
func TestScopedCapabilitiesNeverUnionAcrossEngines(t *testing.T) {
	r := FromProbes(model.File, []model.Engine{model.Qemu, model.Firecracker}, LegacyAll, false, nil)
	r.Engines[1].Features = nil
	h := model.Host{ID: "host", Storage: model.File, Engines: model.List[model.Engine]{model.Qemu, model.Firecracker}, Capabilities: LegacyAll, CapabilityReport: &r}
	if RequireHost(h, model.Firecracker, []Feature{SshBootstrap}) == nil {
		t.Fatal("fell back to legacy or another engine")
	}
	h.CapabilityReport = nil
	h.Capabilities = nil
	if err := RequireHost(h, model.Firecracker, []Feature{Run}); err == nil || !strings.Contains(err.Error(), LegacyFCJailer) {
		t.Fatal(err)
	}
}
func TestFreeBSDOptionsKeepTheirEngineLimits(t *testing.T) {
	keys := []string{"ssh-ed25519 AAAA user"}
	disk := uint32(512)
	if err := ValidateVMOptions(model.Jail, nil, false, keys, []uint16{22}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateVMOptions(model.Bhyve, nil, true, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		e    model.Engine
		disk *uint32
		deny bool
		keys []string
	}{{model.Jail, nil, true, nil}, {model.Bhyve, nil, false, keys}, {model.Jail, &disk, false, nil}, {model.Bhyve, &disk, false, nil}, {model.Docker, nil, true, nil}, {model.Docker, &disk, false, nil}} {
		if ValidateVMOptions(c.e, c.disk, c.deny, c.keys, nil) == nil {
			t.Fatal(c)
		}
	}
}

func TestBackupQualificationIsSeparateFromAdmissionAndRuntimeEligibility(t *testing.T) {
	report := FromProbes(model.Zvol, []model.Engine{model.Bhyve}, LegacyAll, false, nil)
	if report.BackupAdmission || Require(report, model.Bhyve, model.Zvol, Backup) != nil {
		t.Fatal("admission fenced supported recovery")
	}
	reason := "ZFS tools unavailable"
	unqualified := FromProbes(model.Zvol, []model.Engine{model.Bhyve}, LegacyAll, true, &reason)
	if err := Require(unqualified, model.Bhyve, model.Zvol, Backup); err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatal(err)
	}
	if reason := BackupUnsupported(model.Jail, model.File, nil, nil); reason == nil || !strings.Contains(*reason, "unsupported by design") {
		t.Fatal(reason)
	}
}
func TestLegacyGatesRemainEngineAndStorageSpecific(t *testing.T) {
	host := model.Host{ID: "host", Storage: model.File, Engines: model.List[model.Engine]{model.Qemu, model.Firecracker, model.Bhyve, model.Jail}}
	if RequireHost(host, model.Bhyve, []Feature{Run}) != nil || RequireHost(host, model.Bhyve, []Feature{DenyOutgoing}) == nil {
		t.Fatal("bhyve legacy admission")
	}
	host.Capabilities = model.List[string]{LegacyBhyveEgress}
	if RequireHost(host, model.Bhyve, []Feature{DenyOutgoing}) != nil || RequireHost(host, model.Bhyve, []Feature{Isolation}) == nil {
		t.Fatal("bhyve egress versus isolation")
	}
	host.Storage = model.Zvol
	host.Capabilities = model.List[string]{LegacyFCJailer}
	if RequireHost(host, model.Firecracker, []Feature{Run}) == nil {
		t.Fatal("FC zvol without prerequisite")
	}
	host.Capabilities = append(host.Capabilities, LegacyFCZvol)
	if err := RequireHost(host, model.Firecracker, []Feature{Run}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		f   Feature
		e   model.Engine
		tag string
	}{{SshBootstrap, model.Qemu, LegacySSH}, {GuestConfig, model.Firecracker, LegacyConfig}, {Isolation, model.Qemu, LegacyIsolation}, {DiskSize, model.Firecracker, LegacyFCDisk}, {Resources, model.Qemu, LegacyQemuResources}, {Resources, model.Firecracker, LegacyFCResources}, {Backup, model.Qemu, LegacyBackup}} {
		host.Capabilities = slices.Clone(LegacyAll)
		if err := RequireHost(host, c.e, []Feature{c.f}); err != nil {
			t.Fatal(err)
		}
		host.Capabilities = slices.DeleteFunc(host.Capabilities, func(s string) bool { return s == c.tag })
		if RequireHost(host, c.e, []Feature{c.f}) == nil {
			t.Fatal(c)
		}
	}
}
