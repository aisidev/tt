package ctl

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
)

func TestNativeRustGoControllerDatabaseUpgradeBoundary(t *testing.T) {
	oracle := os.Getenv("TT_TEST_CTL_ORACLE")
	if oracle == "" {
		t.Skip("set TT_TEST_CTL_ORACLE to the freshly rebuilt pinned Rust ctl-db example")
	}
	ctx := context.Background()
	type report struct {
		Revision string
		Version  uint32 `json:"schema_version"`
		Hosts    []model.Host
		Envs     []model.Env
		VMs      []model.Vm
	}
	run := func(action, path string) report {
		t.Helper()
		out, err := runcmd.Run(ctx, oracle, action, path)
		if err != nil || !out.Success() {
			t.Fatal(err, string(out.Stderr))
		}
		var result report
		if err := json.Unmarshal(out.Stdout, &result); err != nil {
			t.Fatal(err)
		}
		if result.Revision != "1ad83a9feed5fac820d74b5e7ef752e50a552740" {
			t.Fatal("wrong oracle")
		}
		return result
	}
	readGo := func(path string) report {
		t.Helper()
		db, err := OpenDB(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		version, err := db.version(ctx)
		if err != nil {
			t.Fatal(err)
		}
		vms, err := queryAll[model.Vm](ctx, db.q, "SELECT data FROM vms ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		return report{Version: version, VMs: vms}
	}
	rejectOldReader := func(path string) {
		t.Helper()
		out, err := runcmd.Run(ctx, oracle, "check", path)
		if err != nil || out.Success() {
			t.Fatal("pinned Rust must refuse the upgraded database", err, string(out.Stderr))
		}
	}
	path := filepath.Join(t.TempDir(), "rust.db")
	before := run("create", path)
	if len(before.VMs) != 5 || before.Version != 5 {
		t.Fatal(before)
	}
	db, err := OpenDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	vm, err := db.GetVM(ctx, "qemu")
	if err != nil || vm == nil {
		t.Fatal(vm, err)
	}
	revision := vm.Backup.Revision
	vm.CPU = 3
	vm.Mem = 512
	if err := db.PutVM(ctx, vm); err != nil {
		t.Fatal(err)
	}
	db.Close()
	rejectOldReader(path)
	after := readGo(path)
	found := false
	for _, vm := range after.VMs {
		if vm.ID == "qemu" {
			found = true
			if vm.CPU != 3 || vm.Mem != 512 || vm.Backup.Revision != revision {
				t.Fatal(vm)
			}
		}
	}
	if !found {
		t.Fatal("Go mutation absent")
	}
	goPath := filepath.Join(t.TempDir(), "go.db")
	db, err = OpenDB(ctx, goPath)
	if err != nil {
		t.Fatal(err)
	}
	host := hostRecord("oracle-host")
	env := envRecord("oracle-env")
	created := vmRecord("go-vm", env.ID, host.ID)
	if err := db.PutHost(ctx, &host); err != nil {
		t.Fatal(err)
	}
	if err := db.PutEnvironment(ctx, &env, []model.Vm{created}, nil); err != nil {
		t.Fatal(err)
	}
	db.Close()
	rejectOldReader(goPath)
	checked := readGo(goPath)
	if checked.Version != schemaVersion || len(checked.VMs) != 1 || checked.VMs[0].Backup.Revision != created.Backup.Revision {
		t.Fatal(checked)
	}
	legacy := filepath.Join(t.TempDir(), "v4.db")
	run("v4", legacy)
	db, err = OpenDB(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := db.GetVM(ctx, "firecracker")
	if err != nil {
		t.Fatal(err)
	}
	revision = migrated.Backup.Revision
	db.Close()
	rejectOldReader(legacy)
	checked = readGo(legacy)
	found = false
	for _, vm := range checked.VMs {
		if vm.ID == "firecracker" {
			found = true
			if vm.Backup.Revision != revision {
				t.Fatal("migration revision changed")
			}
		}
	}
	if !found {
		t.Fatal("migrated record missing")
	}
}
