package agent

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aisidev/tt/internal/engine"
	"github.com/aisidev/tt/model"
)

func testDatabase(t *testing.T) *database {
	t.Helper()
	d, err := openAgentDatabase(context.Background(), filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	d.stderr = io.Discard
	t.Cleanup(func() { d.Close() })
	return d
}
func vmFixture(id string) model.Vm {
	return model.Vm{ID: id, EnvID: "env", HostID: "host", Image: "alpine", Engine: model.Qemu, CPU: 2, Mem: 1024, Disk: 40960, IP: "10.10.0.2", State: model.VmStopped, CreatedAt: 1000, Backup: model.NewBackupState()}
}
func execute(t *testing.T, q sqlQuery, text string, args ...any) {
	t.Helper()
	if _, err := q.ExecContext(context.Background(), text, args...); err != nil {
		t.Fatal(err)
	}
}
func TestDBSaveLoadUpsertDeleteAndAbsentVM(t *testing.T) {
	d := testDatabase(t)
	ctx := context.Background()
	vm := vmFixture("vm1")
	if err := saveVM(ctx, d.conn, &vm); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadVM(ctx, d.conn, vm.ID)
	if err != nil || loaded == nil || loaded.ID != vm.ID || loaded.State != vm.State || loaded.CPU != 2 {
		t.Fatal(loaded, err)
	}
	vm.State = model.VmRunning
	if err := saveVM(ctx, d.conn, &vm); err != nil {
		t.Fatal(err)
	}
	all, err := loadAllVMs(ctx, d.conn)
	if err != nil || len(all) != 1 || all[0].State != model.VmRunning {
		t.Fatal(all, err)
	}
	if err := deleteVM(ctx, d.conn, vm.ID); err != nil {
		t.Fatal(err)
	}
	if err := deleteVM(ctx, d.conn, vm.ID); err != nil {
		t.Fatal(err)
	}
	if vm, err := loadVM(ctx, d.conn, "absent"); err != nil || vm != nil {
		t.Fatal(vm, err)
	}
	version, err := schemaVersion(ctx, d.conn)
	if err != nil || version != agentSchemaVersion {
		t.Fatal(version, err)
	}
	for key, want := range map[string]string{"journal_mode": "wal", "synchronous": "2", "busy_timeout": "5000", "foreign_keys": "0"} {
		var got string
		if err := d.conn.QueryRowContext(ctx, "PRAGMA "+key).Scan(&got); err != nil || got != want {
			t.Fatal(key, got, err)
		}
	}
}
func TestIncompatibleDatabasesRejectedBeforeIdentityOrSchemaWrites(t *testing.T) {
	for _, marker := range []string{"missing", "0", "1", "2", "3", "4", "5", "11", "invalid"} {
		t.Run(marker, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.db")
			d, err := openDatabase(context.Background(), path, false, 5000)
			if err != nil {
				t.Fatal(err)
			}
			execute(t, d.conn, "CREATE TABLE _meta (key TEXT PRIMARY KEY,value TEXT NOT NULL);CREATE TABLE vms (id TEXT PRIMARY KEY,data TEXT NOT NULL);INSERT INTO _meta VALUES ('host_id','original-host');INSERT INTO vms VALUES ('retained','original-payload');")
			if marker != "missing" {
				execute(t, d.conn, "INSERT INTO _meta VALUES ('schema_version',?1)", marker)
			}
			d.Close()
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			replacement := "replacement-host"
			if _, err := ResolveHostID(context.Background(), path, &replacement); err == nil {
				t.Fatal("incompatible DB accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("incompatible database changed", err)
			}
		})
	}
}
func TestUnversionedOrIncompleteDatabasesAreNotAutomaticallyRepaired(t *testing.T) {
	for _, setup := range []string{"CREATE TABLE sqliteCustom (value TEXT);", "CREATE TABLE vms (id TEXT PRIMARY KEY,data TEXT NOT NULL);INSERT INTO vms VALUES ('retained','original-payload');", "CREATE TABLE _meta (key TEXT PRIMARY KEY,value TEXT NOT NULL);INSERT INTO _meta VALUES ('schema_version','6');"} {
		d, err := openDatabase(context.Background(), filepath.Join(t.TempDir(), "agent.db"), false, 5000)
		if err != nil {
			t.Fatal(err)
		}
		execute(t, d.conn, setup)
		before, err := textPairs(context.Background(), d.conn, "SELECT type,sql FROM sqlite_schema WHERE sql IS NOT NULL ORDER BY name")
		if err != nil {
			t.Fatal(err)
		}
		if err := d.initialize(context.Background()); err == nil {
			t.Fatal("damaged DB repaired")
		}
		after, err := textPairs(context.Background(), d.conn, "SELECT type,sql FROM sqlite_schema WHERE sql IS NOT NULL ORDER BY name")
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal(before, after, err)
		}
		d.Close()
	}
}
func TestRetainedEnginesSurviveReopenAndUnknownEngineKeepsRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	ctx := context.Background()
	d, err := openAgentDatabase(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	kinds := []model.Engine{model.Qemu, model.Firecracker, model.Docker, model.Bhyve, model.Jail}
	for _, kind := range kinds {
		vm := vmFixture(string(kind))
		vm.Engine = kind
		vm.Options.RequestedDisk = 8192
		if err := saveVM(ctx, d.conn, &vm); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()
	d, err = openAgentDatabase(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var logs bytes.Buffer
	d.stderr = &logs
	for _, kind := range kinds {
		vm, err := loadVM(ctx, d.conn, string(kind))
		if err != nil || vm.Engine != kind || vm.Options.RequestedDisk != 8192 {
			t.Fatal(vm, err)
		}
	}
	raw, err := model.SerdeJSON(vmFixture("unknown"))
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Replace(string(raw), `"engine":"qemu"`, `"engine":"unsupported-engine"`, 1)
	execute(t, d.conn, "INSERT INTO vms VALUES ('unknown',?1)", unknown)
	if _, err := loadVM(ctx, d.conn, "unknown"); err == nil {
		t.Fatal("unknown engine accepted")
	}
	if _, err := loadAllVMs(ctx, d.conn); err == nil {
		t.Fatal("strict list hid unknown record")
	}
	vms, corrupt, err := loadRecoverableVMs(ctx, d.conn, d.stderr)
	if err != nil || !corrupt || len(vms) != 5 {
		t.Fatal(vms, corrupt, err)
	}
	var stored sqlText
	if err := d.conn.QueryRowContext(ctx, "SELECT data FROM vms WHERE id='unknown'").Scan(&stored); err != nil || string(stored) != unknown {
		t.Fatal(stored, err)
	}
	if !strings.Contains(logs.String(), "unreadable VM record unknown; retained, admission disabled") {
		t.Fatal(logs.String())
	}
	execute(t, d.conn, "INSERT INTO vms VALUES ('wrong-id',?1)", string(raw))
	vms, corrupt, err = loadRecoverableVMs(ctx, d.conn, d.stderr)
	if err != nil || !corrupt || len(vms) != 5 {
		t.Fatal(vms, corrupt, err)
	}
}
func TestContainerRuntimeBindingSurvivesReopenAndRejectsSwitching(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	d, err := openAgentDatabase(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	d.stderr = io.Discard
	t.Cleanup(func() { d.Close() })
	podman, docker := engine.PodmanRuntime, engine.DockerRuntime
	selected, err := d.bindContainerRuntime(ctx, &podman, func() *engine.ContainerRuntime { t.Fatal("unexpected detection"); return nil })
	if err != nil || *selected != podman {
		t.Fatal(selected, err)
	}
	vm := vmFixture("container")
	vm.Engine = model.Docker
	if err := saveVM(ctx, d.conn, &vm); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = openAgentDatabase(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	d.stderr = io.Discard
	selected, err = d.bindContainerRuntime(ctx, nil, func() *engine.ContainerRuntime { return &docker })
	if err != nil || *selected != podman {
		t.Fatal(selected, err)
	}
	if _, err := d.bindContainerRuntime(ctx, &docker, func() *engine.ContainerRuntime { return nil }); err == nil {
		t.Fatal("store switched")
	}
	if err := deleteVM(ctx, d.conn, vm.ID); err != nil {
		t.Fatal(err)
	}
	selected, err = d.bindContainerRuntime(ctx, &docker, func() *engine.ContainerRuntime { return nil })
	if err != nil || *selected != docker {
		t.Fatal(selected, err)
	}
	execute(t, d.conn, "DELETE FROM _meta WHERE key='container_runtime';INSERT INTO vms VALUES ('broken','not-json');")
	if _, err := d.bindContainerRuntime(ctx, &podman, func() *engine.ContainerRuntime { return nil }); err == nil {
		t.Fatal("corrupt records adopted")
	}
	selected, err = d.bindContainerRuntime(ctx, nil, func() *engine.ContainerRuntime { t.Fatal("detected with corrupt records"); return &docker })
	if err != nil || selected != nil {
		t.Fatal(selected, err)
	}
}
func TestResolveHostIDGeneratesPersistsAndCLIOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	ctx := context.Background()
	first, err := ResolveHostID(ctx, path, nil)
	if err != nil || len(first) != 8 {
		t.Fatal(first, err)
	}
	second, err := ResolveHostID(ctx, path, nil)
	if err != nil || second != first {
		t.Fatal(second, err)
	}
	custom := "custom-id"
	third, err := ResolveHostID(ctx, path, &custom)
	if err != nil || third != custom {
		t.Fatal(third, err)
	}
	last, err := ResolveHostID(ctx, path, nil)
	if err != nil || last != custom {
		t.Fatal(last, err)
	}
}
func TestSnapshotIsReadableDuringUncommittedMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	ctx := context.Background()
	d, err := openAgentDatabase(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	vm := vmFixture("vm1")
	if err := saveVM(ctx, d.conn, &vm); err != nil {
		t.Fatal(err)
	}
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	vm.State = model.VmRunning
	if err := saveVM(ctx, tx, &vm); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	old, err := ReadVM(bounded, path, "vm1")
	if err != nil || old.State != model.VmStopped {
		t.Fatal(old, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	updated, err := ReadVM(ctx, path, "vm1")
	if err != nil || updated.State != model.VmRunning {
		t.Fatal(updated, err)
	}
}
func TestSQLiteTextDoesNotCoerceCorruptColumnTypes(t *testing.T) {
	d := testDatabase(t)
	for _, value := range []any{[]byte(`{}`), 123, nil} {
		var text sqlText
		err := d.conn.QueryRowContext(context.Background(), "SELECT ?1", value).Scan(&text)
		if err == nil {
			t.Fatalf("coerced %T: %s", value, text)
		}
	}
	var text sqlText
	if err := d.conn.QueryRowContext(context.Background(), "SELECT ?1", "text").Scan(&text); err != nil || text != "text" {
		t.Fatal(text, err)
	}
}
