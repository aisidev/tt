package api_test

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/capability"
	"github.com/aisidev/tt/internal/guestconfig"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

const oracleRevision = "1ad83a9feed5fac820d74b5e7ef752e50a552740"

var wireTypes = map[string]any{
	"Resource": model.Resource{}, "Host": model.Host{}, "Vm": model.Vm{}, "VmResources": model.VmResources{}, "VmOptions": model.VmOptions{}, "Env": model.Env{},
	"SshOptions": model.SshOptions{}, "SshInfo": model.SshInfo{}, "BackupRequest": model.BackupRequest{}, "BackupGeneration": model.BackupGeneration{}, "BackupPending": model.BackupPending{}, "BackupReceipt": model.BackupReceipt{}, "BackupState": model.BackupState{}, "BackupView": api.BackupView{}, "RestoreBody": api.RestoreBody{},
	"CapabilityStatus": model.CapabilityStatus{}, "EngineReport": model.EngineReport{}, "CapabilityReport": model.CapabilityReport{}, "Matrix": model.Matrix{}, "Definition": model.Definition{}, "DesignSupport": model.DesignSupport{}, "Combination": model.Combination{},
	"CreateVmReq": api.CreateVmReq{}, "CreateVmResp": api.CreateVmResp{}, "AgentInfo": api.AgentInfo{}, "VmSpec": api.VmSpec{}, "CreateEnvReq": api.CreateEnvReq{}, "EnvDetail": api.EnvDetail{}, "RegisterHostReq": api.RegisterHostReq{}, "ImageInfo": api.ImageInfo{}, "FleetStatus": api.FleetStatus{}, "RespU64": api.Resp[uint64]{}, "RespUnit": api.Resp[model.Unit]{},
	"Engine": model.Engine(""), "Storage": model.Storage(""), "VmState": model.VmState(""), "EnvState": model.EnvState(""), "HostState": model.HostState(""), "BackupBackend": model.BackupBackend(""), "BackupAction": model.BackupAction(""), "GuestConfig": model.GuestConfig{},
}

type fixture struct {
	Revision        string `json:"revision"`
	CargoLockSHA256 string `json:"cargo_lock_sha256"`
	Cases           []struct {
		Kind, Name, Input string
		OK                bool
		Output            json.RawMessage
	} `json:"cases"`
	Digests []struct {
		Files     model.GuestConfig
		Digest    *string
		Canonical string
	} `json:"digests"`
	Matrix json.RawMessage `json:"matrix"`
	Report json.RawMessage `json:"report"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	data, err := os.ReadFile("testdata/wire.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if f.Revision != oracleRevision || len(f.CargoLockSHA256) != 64 {
		t.Fatal("missing or unexpected oracle provenance")
	}
	return f
}
func semantic(t *testing.T, data []byte) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}
func normalize(t *testing.T, value any) {
	t.Helper()
	switch v := value.(type) {
	case map[string]any:
		if revision, ok := v["revision"].(string); ok {
			if _, err := uuid.Parse(revision); err != nil {
				t.Fatal("invalid default revision", err)
			}
			v["revision"] = "00000000-0000-4000-8000-000000000001"
		}
		for _, child := range v {
			normalize(t, child)
		}
	case []any:
		for _, child := range v {
			normalize(t, child)
		}
	}
}
func TestRustWireFixtures(t *testing.T) {
	f := loadFixture(t)
	covered := map[string]bool{}
	for _, c := range f.Cases {
		t.Run(c.Kind+"/"+c.Name, func(t *testing.T) {
			zero, exists := wireTypes[c.Kind]
			if !exists {
				t.Fatal("unregistered fixture type", c.Kind)
			}
			covered[c.Kind] = true
			value := reflect.New(reflect.TypeOf(zero)).Interface()
			err := json.Unmarshal([]byte(c.Input), value)
			if (err == nil) != c.OK {
				t.Fatalf("Rust accepted=%v, Go error=%v (input in fixture)", c.OK, err)
			}
			if err != nil {
				return
			}
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			got := semantic(t, data)
			normalize(t, got)
			want := semantic(t, c.Output)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Go=%s\nRust=%s", data, c.Output)
			}
		})
	}
	for kind := range wireTypes {
		if !covered[kind] {
			t.Errorf("no Rust fixtures for %s", kind)
		}
	}
}
func TestRustGuestConfigDigestVectors(t *testing.T) {
	for _, c := range loadFixture(t).Digests {
		if got := string(guestconfig.CanonicalJSON(c.Files)); got != c.Canonical {
			t.Errorf("canonical=%q want %q", got, c.Canonical)
		}
		if got := guestconfig.Digest(c.Files); !reflect.DeepEqual(got, c.Digest) {
			t.Errorf("digest=%v want %v", got, c.Digest)
		}
	}
}
func neutralizeFreeBSDExperimentalLabel(value any) {
	switch v := value.(type) {
	case map[string]any:
		if v["platform"] == "freebsd" {
			v["experimental"] = false
		}
		if limit, ok := v["limit"].(string); ok && (limit == "Experimental FreeBSD; manual host and guest setup required" || limit == "Validated only on FreeBSD 15.x; manual host and guest setup required") {
			v["limit"] = "freebsd-15-run-limit"
		}
		for _, child := range v {
			neutralizeFreeBSDExperimentalLabel(child)
		}
	case []any:
		for _, child := range v {
			neutralizeFreeBSDExperimentalLabel(child)
		}
	}
}

func TestRustCapabilityMatrixAndReport(t *testing.T) {
	f := loadFixture(t)
	for _, c := range []struct {
		value any
		want  json.RawMessage
	}{
		{capability.Matrix(), f.Matrix},
		{capability.FromProbes(model.File, []model.Engine{model.Qemu, model.Firecracker}, capability.LegacyAll, true, nil), f.Report},
	} {
		data, err := json.Marshal(c.value)
		if err != nil {
			t.Fatal(err)
		}
		got, want := semantic(t, data), semantic(t, c.want)
		neutralizeFreeBSDExperimentalLabel(got)
		neutralizeFreeBSDExperimentalLabel(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("capability output differs from Rust")
		}
	}
}
func TestApiRespEmptyOK(t *testing.T) {
	data, err := json.Marshal(api.Empty())
	if err != nil || string(data) != `{"ok":true}` {
		t.Fatalf("%s %v", data, err)
	}
}
func TestVmSpecDefaults(t *testing.T) {
	var spec api.VmSpec
	if err := json.Unmarshal([]byte(`{"image":"ubuntu"}`), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Engine != model.Qemu || len(spec.Ports) != 0 || spec.DenyOutgoing {
		t.Fatal(spec)
	}
}
