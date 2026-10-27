package engine

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/aisidev/tt/model"
)

func TestLinuxEngineRustGoldenCommandsAndConfiguration(t *testing.T) {
	fixture := readGolden(t)
	vm := testVM()
	q := newQemu()
	q.runDir = t.TempDir()
	cmd, err := q.command(vm, "/retained/disk", "raw")
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string(nil), cmd.Args...)
	for i := range args {
		args[i] = strings.ReplaceAll(args[i], q.runDir, model.RunDir)
	}
	if !reflect.DeepEqual(args, fixture.Qemu) {
		t.Fatalf("QEMU\ngot %q\nwant %q", args, fixture.Qemu)
	}
	network, err := qemuNetworkConfig(vm)
	if err != nil || network != fixture.QemuNetwork {
		t.Fatal(network, err)
	}
	d := &Docker{runtime: PodmanRuntime}
	if !reflect.DeepEqual(d.command(vm).Args, fixture.Docker) {
		t.Fatal(d.command(vm).Args)
	}
	vm.Engine = model.Firecracker
	for _, configured := range []bool{false, true} {
		want := fixture.Firecracker
		if configured {
			vm.Options.SSH = &model.SshOptions{}
			want = fixture.FirecrackerConfigured
		}
		data, err := model.SerdeJSON(firecrackerConfiguration(vm, "02:54:0a:0a:01:02"))
		if err != nil {
			t.Fatal(err)
		}
		var a, b any
		if err := json.Unmarshal(data, &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(want, &b); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("Firecracker\ngot %s\nwant %s", data, want)
		}
		if configured && string(data) != fixture.FirecrackerBytes {
			t.Fatalf("serde bytes differ:\n%s\n%s", data, fixture.FirecrackerBytes)
		}
	}
}
