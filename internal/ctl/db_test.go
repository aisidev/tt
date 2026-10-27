package ctl

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"testing"

	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	db, err := OpenDB(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.stderr = io.Discard
	t.Cleanup(func() { db.Close() })
	return db
}
func hostRecord(id string) model.Host {
	return model.Host{ID: id, Addr: id + ":9100", Resource: model.Resource{CPUTotal: 8, MemTotal: 16384, DiskTotal: 500000}, State: model.HostOnline, Engines: model.List[model.Engine]{model.Qemu}, Storage: model.File, RegisteredAt: 1000}
}
func envRecord(id string) model.Env {
	return model.Env{ID: id, Owner: "tester", VmIds: model.List[string]{"vm1"}, CreatedAt: 1000, ExpiresAt: 2000, State: model.EnvActive}
}
func vmRecord(id, env, host string) model.Vm {
	return model.Vm{ID: id, EnvID: env, HostID: host, Image: "ubuntu", Engine: model.Qemu, CPU: 2, Mem: 1024, Disk: 40960, IP: "10.10.0.1", State: model.VmRunning, CreatedAt: 1000, Backup: model.NewBackupState()}
}
func execSQL(t *testing.T, db *DB, query string, args ...any) {
	t.Helper()
	if _, err := db.q.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}
func TestHostEnvironmentAndVMCRUD(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	host := hostRecord("h1")
	env := envRecord("env1")
	vm := vmRecord("vm1", env.ID, host.ID)
	if err := db.PutHost(ctx, &host); err != nil {
		t.Fatal(err)
	}
	if err := db.PutEnvironment(ctx, &env, []model.Vm{vm}, nil); err != nil {
		t.Fatal(err)
	}
	if count, err := db.HostCount(ctx); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if count, err := db.EnvCount(ctx); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if count, err := db.VMCount(ctx); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	got, err := db.GetHost(ctx, host.ID)
	if err != nil || got.ID != host.ID || got.Resource.CPUTotal != 8 {
		t.Fatal(got, err)
	}
	host.State = model.HostOffline
	if err := db.PutHost(ctx, &host); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetHost(ctx, host.ID)
	if err != nil || got.State != model.HostOffline {
		t.Fatal(got, err)
	}
	byEnv, err := db.VMsByEnv(ctx, env.ID)
	if err != nil || len(byEnv) != 1 {
		t.Fatal(byEnv, err)
	}
	byHost, err := db.VMsByHost(ctx, host.ID)
	if err != nil || len(byHost) != 1 {
		t.Fatal(byHost, err)
	}
	if err := db.RemoveVM(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveVM(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := db.GetVM(ctx, vm.ID); err != nil || got != nil {
		t.Fatal(got, err)
	}
	if err := db.RemoveEnv(ctx, env.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveHost(ctx, host.ID); err != nil {
		t.Fatal(err)
	}
	if hosts, err := db.ListHosts(ctx); err != nil || len(hosts) != 0 {
		t.Fatal(hosts, err)
	}
	if envs, err := db.ListEnvs(ctx); err != nil || len(envs) != 0 {
		t.Fatal(envs, err)
	}
}
func TestMigrationAssignsStableRevisionsAndRollsBackBadRecords(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		db := testDB(t)
		ctx := context.Background()
		execSQL(t, db, "UPDATE _meta SET value='4' WHERE key='schema_version';DROP TABLE vms;CREATE TABLE vms (id TEXT PRIMARY KEY,data TEXT NOT NULL);INSERT INTO vms VALUES ('a','{\"id\":\"a\",\"future\":18446744073709551615}');")
		if corrupt {
			execSQL(t, db, "INSERT INTO vms VALUES ('b','not-json')")
		}
		err := db.migrate(ctx)
		var raw textColumn
		if scanErr := db.q.QueryRowContext(ctx, "SELECT data FROM vms WHERE id='a'").Scan(&raw); scanErr != nil {
			t.Fatal(scanErr)
		}
		version, versionErr := db.version(ctx)
		if versionErr != nil {
			t.Fatal(versionErr)
		}
		if corrupt {
			if err == nil || version != 4 || raw != `{"id":"a","future":18446744073709551615}` {
				t.Fatal(err, version, raw)
			}
		} else {
			if err != nil || version != schemaVersion {
				t.Fatal(err, version)
			}
			value, err := model.DecodeJSONValue([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			record := value.(map[string]any)
			revision := record["backup"].(map[string]any)["revision"].(string)
			if _, err := uuid.Parse(revision); err != nil {
				t.Fatal(err)
			}
			if record["future"] != json.Number("18446744073709551615") {
				t.Fatal(record)
			}
			if err := db.migrate(ctx); err != nil {
				t.Fatal(err)
			}
			var again textColumn
			if err := db.q.QueryRowContext(ctx, "SELECT data FROM vms WHERE id='a'").Scan(&again); err != nil || again != raw {
				t.Fatal(again, err)
			}
		}
	}
}
func TestMigrationPreservesExistingBackupAndRejectsNewerSchema(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	execSQL(t, db, "UPDATE _meta SET value='4' WHERE key='schema_version';INSERT INTO vms VALUES ('a','e','h','{\"backup\":null,\"future\":true}');")
	if err := db.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var raw textColumn
	if err := db.q.QueryRowContext(ctx, "SELECT data FROM vms WHERE id='a'").Scan(&raw); err != nil || raw != `{"backup":null,"future":true}` {
		t.Fatal(raw, err)
	}
	execSQL(t, db, "UPDATE _meta SET value='10' WHERE key='schema_version'")
	if err := db.migrate(ctx); err == nil || err.Error() != "database schema v10 is newer than this binary (v9); upgrade TTstack first" {
		t.Fatal(err)
	}
}
func TestEngineSetsRoundtripAndUnknownRecordsFailWithoutMutation(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	host := hostRecord("host")
	host.Engines = model.List[model.Engine]{model.Qemu, model.Firecracker, model.Docker, model.Bhyve, model.Jail}
	if err := db.PutHost(ctx, &host); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetHost(ctx, host.ID)
	if err != nil || !reflect.DeepEqual(got.Engines, host.Engines) {
		t.Fatal(got, err)
	}
	for _, kind := range host.Engines {
		vm := vmRecord(string(kind), "env", "host")
		vm.Engine = kind
		vm.State = model.VmStopped
		if err := db.PutVM(ctx, &vm); err != nil {
			t.Fatal(err)
		}
		got, err := db.GetVM(ctx, vm.ID)
		if err != nil || got.Engine != kind {
			t.Fatal(got, err)
		}
	}
	data, err := model.SerdeJSON(host)
	if err != nil {
		t.Fatal(err)
	}
	value, err := model.DecodeJSONValue(data)
	if err != nil {
		t.Fatal(err)
	}
	value.(map[string]any)["engines"] = []string{"unsupported-engine"}
	raw, err := model.SerdeJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, "UPDATE hosts SET data=?1 WHERE id='host'", string(raw))
	if _, err := db.GetHost(ctx, "host"); err == nil {
		t.Fatal("unknown engine accepted")
	}
	if _, err := db.ListHosts(ctx); err == nil {
		t.Fatal("strict list ignored corruption")
	}
	recovery, corrupt, err := db.RecoveryHosts(ctx)
	if err != nil || len(recovery) != 0 {
		t.Fatal(recovery, err)
	}
	if len(corrupt) != 1 || corrupt[0] != "host" {
		t.Fatal(corrupt)
	}
	var retained textColumn
	if err := db.q.QueryRowContext(ctx, "SELECT data FROM hosts WHERE id='host'").Scan(&retained); err != nil || string(retained) != string(raw) {
		t.Fatal(retained, err)
	}
	if count, err := db.VMCount(ctx); err != nil || count != 5 {
		t.Fatal(count, err)
	}
}
func TestEnvironmentPlanIsAtomicBeforeAgentContact(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	execSQL(t, db, "CREATE TRIGGER reject_second BEFORE INSERT ON vms WHEN NEW.id='bad' BEGIN SELECT RAISE(ABORT,'injected plan failure');END;")
	env := envRecord("planned")
	if err := db.PutEnvironment(ctx, &env, []model.Vm{vmRecord("good", env.ID, "host"), vmRecord("bad", env.ID, "host")}, nil); err == nil {
		t.Fatal("injected failure hidden")
	}
	if count, err := db.VMCount(ctx); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if value, err := db.GetEnv(ctx, env.ID); err != nil || value != nil {
		t.Fatal(value, err)
	}
}
func TestBackupObservationCommitsVMAndReservationTogether(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	host := hostRecord("host")
	vm := vmRecord("vm", "env", host.ID)
	vm.Disk = 16
	host.Resource.DiskUsed = 16
	if err := db.PutHost(ctx, &host); err != nil {
		t.Fatal(err)
	}
	if err := db.PutVM(ctx, &vm); err != nil {
		t.Fatal(err)
	}
	vm.Backup.Current = &model.BackupGeneration{ID: uuid.NewString(), Backend: model.BackupReflink, RootBytes: 16 * 1024 * 1024}
	if err := db.PutVMObservation(ctx, &vm, 16); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetHost(ctx, host.ID)
	if err != nil || got.Resource.DiskUsed != 32 {
		t.Fatal(got, err)
	}
	execSQL(t, db, "CREATE TRIGGER reject_host BEFORE INSERT ON hosts BEGIN SELECT RAISE(ABORT,'injected host failure');END;")
	previous := vm.Backup.Revision
	vm.Backup.Revision = uuid.NewString()
	if err := db.PutVMObservation(ctx, &vm, 32); err == nil {
		t.Fatal("host failure hidden")
	}
	stored, err := db.GetVM(ctx, vm.ID)
	if err != nil || stored.Backup.Revision != previous {
		t.Fatal(stored, err)
	}
}
func TestDetachHostReturnsOrphansAndOnlyRemovesTracking(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	host := hostRecord("lost")
	env := envRecord("env")
	env.VmIds = model.List[string]{"orphan", "retained"}
	if err := db.PutHost(ctx, &host); err != nil {
		t.Fatal(err)
	}
	if err := db.PutEnvironment(ctx, &env, []model.Vm{vmRecord("orphan", env.ID, host.ID), vmRecord("retained", env.ID, "online")}, nil); err != nil {
		t.Fatal(err)
	}
	orphans, err := db.DetachHost(ctx, host.ID)
	if err != nil || len(orphans) != 1 || orphans[0].ID != "orphan" {
		t.Fatal(orphans, err)
	}
	updated, err := db.GetEnv(ctx, env.ID)
	if err != nil || updated.State != model.EnvFailed || !reflect.DeepEqual(updated.VmIds, model.List[string]{"retained"}) {
		t.Fatal(updated, err)
	}
	if h, err := db.GetHost(ctx, host.ID); err != nil || h != nil {
		t.Fatal(h, err)
	}
}
func TestFleetStatusAggregatesOnlyOnlineCapacity(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	h1, h2 := hostRecord("h1"), hostRecord("h2")
	h1.Resource.CPUUsed = 2
	h1.Resource.MemUsed = 4096
	h2.State = model.HostOffline
	for _, host := range []*model.Host{&h1, &h2} {
		if err := db.PutHost(ctx, host); err != nil {
			t.Fatal(err)
		}
	}
	env := envRecord("env")
	if err := db.PutEnvironment(ctx, &env, []model.Vm{vmRecord("vm", env.ID, h1.ID)}, nil); err != nil {
		t.Fatal(err)
	}
	status, err := db.FleetStatus(ctx)
	if err != nil || status.Hosts != 2 || status.HostsOnline != 1 || status.TotalEnvs != 1 || status.TotalVMs != 1 || status.CPUTotal != 8 || status.CPUUsed != 2 {
		t.Fatal(status, err)
	}
}

func TestEnvironmentPlanRollsBackHostReservationsTogether(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	host := hostRecord("host")
	if err := db.PutHost(ctx, &host); err != nil {
		t.Fatal(err)
	}
	env := envRecord("planned")
	vm := vmRecord("vm", env.ID, host.ID)
	host.Resource.Account(vm)
	// INSERT OR REPLACE fires the INSERT trigger even over an existing row.
	execSQL(t, db, "CREATE TRIGGER reject_host BEFORE INSERT ON hosts BEGIN SELECT RAISE(ABORT,'injected host failure');END;")
	if err := db.PutEnvironment(ctx, &env, []model.Vm{vm}, []model.Host{host}); err == nil {
		t.Fatal("injected host failure hidden")
	}
	if value, err := db.GetEnv(ctx, env.ID); err != nil || value != nil {
		t.Fatal(value, err)
	}
	if count, err := db.VMCount(ctx); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	got, err := db.GetHost(ctx, host.ID)
	if err != nil || got.Resource.CPUUsed != 0 || got.Resource.MemUsed != 0 || got.Resource.DiskUsed != 0 {
		t.Fatal("host reservation leaked through a rolled-back plan", got.Resource)
	}
}
func TestRemoveVMReleaseReturnsReservationsAtomically(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	host := hostRecord("host")
	env := envRecord("env")
	vm := vmRecord("vm", env.ID, host.ID)
	host.Resource.Account(vm)
	if err := db.PutHost(ctx, &host); err != nil {
		t.Fatal(err)
	}
	if err := db.PutEnv(ctx, &env); err != nil {
		t.Fatal(err)
	}
	if err := db.PutVM(ctx, &vm); err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, "CREATE TRIGGER reject_host BEFORE INSERT ON hosts BEGIN SELECT RAISE(ABORT,'injected host failure');END;")
	if err := db.RemoveVMRelease(ctx, vm); err == nil {
		t.Fatal("host failure hidden")
	}
	if got, err := db.GetVM(ctx, vm.ID); err != nil || got == nil {
		t.Fatal("VM row must survive a failed release transaction", got, err)
	}
	execSQL(t, db, "DROP TRIGGER reject_host")
	if err := db.RemoveVMRelease(ctx, vm); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetHost(ctx, host.ID)
	if err != nil || got.Resource.CPUUsed != 0 || got.Resource.MemUsed != 0 || got.Resource.DiskUsed != 0 || got.Resource.VmCount != 0 {
		t.Fatal("reservations not returned", got.Resource)
	}
	if v, err := db.GetVM(ctx, vm.ID); err != nil || v != nil {
		t.Fatal(v, err)
	}
}
func TestFleetListingsStayIdOrderedAcrossRewrites(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	for _, id := range []string{"host-b", "host-a", "host-c"} {
		host := hostRecord(id)
		if err := db.PutHost(ctx, &host); err != nil {
			t.Fatal(err)
		}
	}
	// INSERT OR REPLACE rewrites move the row to the end of the table.
	host := hostRecord("host-a")
	if err := db.PutHost(ctx, &host); err != nil {
		t.Fatal(err)
	}
	hosts, err := db.ListHosts(ctx)
	if err != nil || len(hosts) != 3 {
		t.Fatal(hosts, err)
	}
	if hosts[0].ID != "host-a" || hosts[1].ID != "host-b" || hosts[2].ID != "host-c" {
		t.Fatal(hosts)
	}
	env := envRecord("env")
	if err := db.PutEnv(ctx, &env); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"vm-b", "vm-a"} {
		vm := vmRecord(id, env.ID, "host-a")
		if err := db.PutVM(ctx, &vm); err != nil {
			t.Fatal(err)
		}
	}
	vm := vmRecord("vm-a", env.ID, "host-a")
	if err := db.PutVM(ctx, &vm); err != nil {
		t.Fatal(err)
	}
	vms, err := db.VMsByEnv(ctx, env.ID)
	if err != nil || len(vms) != 2 || vms[0].ID != "vm-a" || vms[1].ID != "vm-b" {
		t.Fatal(vms, err)
	}
}
