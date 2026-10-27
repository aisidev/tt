package model

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestEngineDisplayRoundtrip(t *testing.T) {
	for _, e := range []Engine{Qemu, Firecracker, Bhyve, Docker, Jail} {
		got, err := ParseEngine(e.String())
		if err != nil || got != e {
			t.Fatalf("%s %v", got, err)
		}
	}
}
func TestEngineAliases(t *testing.T) {
	for input, want := range map[string]Engine{"kvm": Qemu, "fc": Firecracker, "podman": Docker, "QEMU": Qemu} {
		got, err := ParseEngine(input)
		if err != nil || got != want {
			t.Errorf("%s: %s %v", input, got, err)
		}
	}
}
func TestEngineSerdeJSON(t *testing.T) {
	for _, input := range []string{`"unknown-engine"`, `"kvm"`, `"fc"`, `"podman"`, `"QEMU"`, `""`, `null`} {
		var v Engine
		if err := json.Unmarshal([]byte(input), &v); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
}
func TestResourceSaturatesAndRetainsStoppedDisk(t *testing.T) {
	r := Resource{CPUTotal: 1, CPUUsed: 2, MemTotal: 2, MemUsed: 3, DiskTotal: 3, DiskUsed: 4}
	if r.CPUFree() != 0 || r.MemFree() != 0 || r.DiskFree() != 0 || r.CanFit(1, 0, 0) {
		t.Fatal(r)
	}
	r = Resource{}
	v := Vm{Engine: Firecracker, CPU: 1, Mem: 256, Disk: 512, State: VmStopped}
	r.Account(v)
	if r.CPUUsed != 0 || r.MemUsed != 0 || r.DiskUsed != 512 || r.VmCount != 1 {
		t.Fatal(r)
	}
	v.State = VmFailed
	r.Account(v)
	if r.CPUUsed != 1 || r.MemUsed != 384 || r.DiskUsed != 1024 {
		t.Fatal(r)
	}
	r.CPUUsed = math.MaxUint32
	r.Account(v)
	if r.CPUUsed != math.MaxUint32 {
		t.Fatal("overflow")
	}
}
func TestBackupReservationUsesMaxPendingAndForwarded(t *testing.T) {
	s := BackupState{Current: &BackupGeneration{RootBytes: 1024*1024 + 1}, Retired: List[BackupGeneration]{{RootBytes: 1}}, Pending: &BackupPending{Request: BackupRequest{Action: BackupCreate}, Artifact: BackupGeneration{RootBytes: 5 * 1024 * 1024}}, ForwardingReservedMiB: 7}
	if s.ReservedMiB() != 10 {
		t.Fatal(s.ReservedMiB())
	}
	s.Pending.Request.Action = BackupDelete
	s.ForwardingReservedMiB = 0
	if s.ReservedMiB() != 3 {
		t.Fatal(s.ReservedMiB())
	}
	if (BackupGeneration{RootBytes: math.MaxUint64}).ReservedMiB() != math.MaxUint32 {
		t.Fatal("large disk did not saturate")
	}
}
func TestImageNamesRespectBackendFormat(t *testing.T) {
	for _, name := range []string{"ubuntu:24.04", "registry.example:5000/team/app:v1", "alpine@sha256:abcdef"} {
		if ValidateImage(name, Docker) != nil || ValidateImage(name, Qemu) == nil {
			t.Fatal(name)
		}
	}
	if ValidateImage("--privileged", Docker) == nil || ValidateImage("../image", Qemu) == nil {
		t.Fatal("unsafe image accepted")
	}
}
func TestValidateName(t *testing.T) {
	for _, s := range []string{"", ".hidden", "a..b", "a/b", "bad!", strings.Repeat("a", 129), "é"} {
		if ValidateName(s, "env") == nil {
			t.Error(s)
		}
	}
	for _, s := range []string{"vm-1", "VM_2", "a.b"} {
		if err := ValidateName(s, "env"); err != nil {
			t.Error(err)
		}
	}
}
func TestValidatesAccountWithoutEchoingPrivateInputs(t *testing.T) {
	for _, user := range []string{"", "-root", "user;id", "user\nroot", "../root", "user name"} {
		err := (SshOptions{User: user}).Validate()
		if err == nil {
			t.Fatal(user)
		}
	}
	if err := (SshOptions{User: "user", Sudo: true}).Validate(); err != nil {
		t.Fatal(err)
	}
}
func TestJSONRejectsMalformedStringsAndNullElements(t *testing.T) {
	for _, input := range []string{`{"user":"\ud800"}`, `{"user":"\udc00"}`, "{\"user\":\"\xff\"}", `{"user":null}`} {
		var o SshOptions
		if json.Unmarshal([]byte(input), &o) == nil {
			t.Fatalf("accepted %q", input)
		}
	}
	var list List[string]
	if json.Unmarshal([]byte(`[null]`), &list) == nil {
		t.Fatal("accepted null string")
	}
}

func TestCLIEnumCaseFoldingIsASCIIOnly(t *testing.T) {
	for _, name := range []string{"Kvm", "docKer", "unknown"} {
		if _, err := ParseEngine(name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	for _, name := range []string{"FİLE", "unknown"} {
		if _, err := ParseStorage(name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	for _, name := range []string{"FILE", "ZVOL"} {
		if _, err := ParseStorage(name); err != nil {
			t.Fatal(err)
		}
	}
}
