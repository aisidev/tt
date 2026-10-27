package agent

import (
	"context"
	"net/netip"
	"reflect"
	"testing"

	"github.com/aisidev/tt/internal/engine"
	"github.com/aisidev/tt/model"
)

func TestNetworkBindingPreventsLosingOwnedIngressDuringReconfiguration(t *testing.T) {
	d := testDatabase(t)
	ctx := context.Background()
	ingress := &SshIngress{PublicAddress: netip.MustParseAddr("192.0.2.1")}
	if err := d.bindNetwork(ctx, ingress, 20000, 20100); err != nil {
		t.Fatal(err)
	}
	vm := vmFixture("owned")
	if err := saveVM(ctx, d.conn, &vm); err != nil {
		t.Fatal(err)
	}
	if err := d.bindNetwork(ctx, ingress, 20000, 20100); err != nil {
		t.Fatal(err)
	}
	if err := d.bindNetwork(ctx, nil, 20000, 20100); err == nil {
		t.Fatal("ingress changed with VM retained")
	}
	if err := d.bindNetwork(ctx, ingress, 20100, 20200); err == nil {
		t.Fatal("port range changed with VM retained")
	}
	if err := deleteVM(ctx, d.conn, vm.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.bindNetwork(ctx, nil, 20100, 20200); err != nil {
		t.Fatal(err)
	}
	for _, ports := range [][2]uint16{{0, 100}, {101, 100}} {
		if d.bindNetwork(ctx, nil, ports[0], ports[1]) == nil {
			t.Fatal("invalid port range")
		}
	}
}
func TestMissingNativeBindingsAreRejectedInsteadOfAdopted(t *testing.T) {
	d := testDatabase(t)
	ctx := context.Background()
	vm := vmFixture("container")
	vm.Engine = model.Docker
	if err := saveVM(ctx, d.conn, &vm); err != nil {
		t.Fatal(err)
	}
	if err := d.bindNetwork(ctx, nil, 20000, 65535); err == nil {
		t.Fatal("missing network binding adopted")
	}
	requested := engine.DockerRuntime
	if _, err := d.bindContainerRuntime(ctx, &requested, func() *engine.ContainerRuntime { return &requested }); err == nil {
		t.Fatal("missing container binding adopted")
	}
}
func TestCorruptVMRowDoesNotBlockExistingNativeBindings(t *testing.T) {
	d := testDatabase(t)
	ctx := context.Background()
	requested := engine.PodmanRuntime
	if err := d.bindNetwork(ctx, nil, 20000, 65535); err != nil {
		t.Fatal(err)
	}
	if _, err := d.bindContainerRuntime(ctx, &requested, func() *engine.ContainerRuntime { return nil }); err != nil {
		t.Fatal(err)
	}
	execute(t, d.conn, "INSERT INTO vms VALUES ('broken','not-json')")
	if err := d.bindNetwork(ctx, nil, 20000, 65535); err != nil {
		t.Fatal(err)
	}
	if _, err := d.bindContainerRuntime(ctx, nil, func() *engine.ContainerRuntime { t.Fatal("unexpected detection"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := d.bindNetwork(ctx, nil, 20000, 20100); err == nil {
		t.Fatal("corrupt row released binding")
	}
}
func TestPortsReuseHolesSkipConflictsAndFailExplicitlyWhenExhausted(t *testing.T) {
	vm := vmFixture("existing")
	vm.PortMap = model.Map[uint16, uint16]{22: 20001}
	got, err := allocatePorts([]model.Vm{vm}, []uint16{80, 22, 80}, 20000, 20004, func(p uint16) bool { return p != 20002 })
	want := model.Map[uint16, uint16]{80: 20000, 22: 20003}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal(got, err)
	}
	if _, err := allocatePorts([]model.Vm{vm}, []uint16{80, 22, 443}, 20000, 20002, func(uint16) bool { return true }); err == nil || err.Error() != "host TCP port pool exhausted" {
		t.Fatal(err)
	}
	got, err = allocatePorts(nil, []uint16{22}, 65535, 65535, func(uint16) bool { return true })
	if err != nil || got[22] != 65535 {
		t.Fatal(got, err)
	}
	if _, err := allocatePorts(nil, []uint16{22, 80}, 65535, 65535, func(uint16) bool { return true }); err == nil {
		t.Fatal("port counter wrapped")
	}
}
