package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
)

func TestNativeRustGoAgentDatabaseUpgradeBoundary(t *testing.T) {
	oracle := os.Getenv("TT_TEST_AGENT_ORACLE")
	if oracle == "" {
		t.Skip("set TT_TEST_AGENT_ORACLE to the freshly rebuilt pinned Rust agent-db example")
	}
	ctx := context.Background()
	type report struct {
		Revision string     `json:"revision"`
		HostID   string     `json:"host_id"`
		VMs      []model.Vm `json:"vms"`
		Version  uint32     `json:"schema_version"`
	}
	run := func(action, path string) report {
		t.Helper()
		out, err := runcmd.Run(ctx, oracle, action, path)
		if err != nil || !out.Success() {
			t.Fatalf("Rust oracle %s: %v %s", action, err, out.Stderr)
		}
		var result report
		if err := json.Unmarshal(out.Stdout, &result); err != nil {
			t.Fatal(err)
		}
		if result.Revision != "1ad83a9feed5fac820d74b5e7ef752e50a552740" {
			t.Fatal("wrong Rust oracle")
		}
		return result
	}
	// The extensions migrate forward; the pinned Rust cannot reopen v10.
	readGo := func(path string) report {
		t.Helper()
		d, err := openAgentDatabase(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		result := report{Version: agentSchemaVersion}
		if err := d.conn.QueryRowContext(ctx, "SELECT value FROM _meta WHERE key='host_id'").Scan(&result.HostID); err != nil {
			t.Fatal(err)
		}
		result.VMs, err = loadAllVMs(ctx, d.conn)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	rejectOldReader := func(path string) {
		t.Helper()
		out, err := runcmd.Run(ctx, oracle, "check", path)
		if err != nil || out.Success() {
			t.Fatal("pinned Rust must refuse the upgraded database", err, string(out.Stderr))
		}
	}
	source := filepath.Join(t.TempDir(), "rust.db")
	initial := run("create", source)
	if len(initial.VMs) != 5 || initial.Version != 6 {
		t.Fatal(initial)
	}
	d, err := openAgentDatabase(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	vm, err := loadVM(ctx, d.conn, "firecracker")
	if err != nil || vm == nil {
		t.Fatal(vm, err)
	}
	revision := vm.Backup.Revision
	vm.CPU = 3
	vm.Mem = 512
	if err := saveVM(ctx, d.conn, vm); err != nil {
		t.Fatal(err)
	}
	d.Close()
	rejectOldReader(source)
	checked := readGo(source)
	found := false
	for _, vm := range checked.VMs {
		if vm.ID == "firecracker" {
			found = true
			if vm.CPU != 3 || vm.Mem != 512 || vm.Backup.Revision != revision {
				t.Fatal(vm)
			}
		}
	}
	if !found {
		t.Fatal("mutated VM absent")
	}
	goPath := filepath.Join(t.TempDir(), "go.db")
	host := "go-host"
	if _, err := ResolveHostID(ctx, goPath, &host); err != nil {
		t.Fatal(err)
	}
	d, err = openAgentDatabase(ctx, goPath)
	if err != nil {
		t.Fatal(err)
	}
	goVM := vmFixture("go-vm")
	goVM.HostID = host
	if err := saveVM(ctx, d.conn, &goVM); err != nil {
		t.Fatal(err)
	}
	d.Close()
	rejectOldReader(goPath)
	goChecked := readGo(goPath)
	if goChecked.HostID != host || len(goChecked.VMs) != 1 || goChecked.VMs[0].Backup.Revision != goVM.Backup.Revision {
		t.Fatal(goChecked)
	}
	oldPath := filepath.Join(t.TempDir(), "v5.db")
	run("v5", oldPath)
	converted := filepath.Join(filepath.Dir(oldPath), "v6.db")
	if count, err := ConvertV5(ctx, oldPath, converted); err != nil || count != 5 {
		t.Fatal(count, err)
	}
	convertedReport := run("check", converted)
	if convertedReport.Version != 6 || len(convertedReport.VMs) != 5 {
		t.Fatal(convertedReport)
	}
	// Exercise the reverse converter too: Rust converts a Go-created v5 database,
	// and the native Go reader accepts the resulting v6 rows without adaptation.
	reverseSource := v5Fixture(t, "5", false)
	reverseOutput := filepath.Join(filepath.Dir(reverseSource), "rust-converted.db")
	converter := filepath.Join(filepath.Dir(oracle), "upgrade-agent-v5")
	out, err := runcmd.Run(ctx, converter, "--source", reverseSource, "--output", reverseOutput)
	if err != nil || !out.Success() {
		t.Fatal(err, string(out.Stderr))
	}
	reverse, err := ReadVMs(ctx, reverseOutput)
	if err != nil || len(reverse) != 1 || reverse[0].ReservedDisk() != 8196 {
		t.Fatal(reverse, err)
	}
}
