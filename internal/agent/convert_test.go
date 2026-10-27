package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
)

func v5Fixture(t *testing.T, version string, invalid bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.db")
	d, err := openDatabase(context.Background(), path, false, 5000)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	execute(t, d.conn, "CREATE TABLE _meta (key TEXT PRIMARY KEY,value TEXT NOT NULL);CREATE TABLE vms (id TEXT PRIMARY KEY,data TEXT NOT NULL);")
	execute(t, d.conn, "INSERT INTO _meta VALUES ('schema_version',?1),('host_id','retained-host'),('network_config','retained-network')", version)
	record := `{"id":"retained-vm","env_id":"retained-env","host_id":"retained-host","image":"retained-image","engine":"firecracker","cpu":2,"mem":1024,"disk":8196,"ip":"10.10.0.2","port_map":{"22":20022},"state":"running","created_at":123,"options":{"ports":[22],"ssh_keys":["fixture-public-key"],"deny_outgoing":false,"requested_disk":8192,"isolated_network":true,"guest_config_digest":"retained-digest"},"future_opaque_field":{"message":"<&>","integer":18446744073709551615}}`
	if invalid {
		record = strings.Replace(record, `"firecracker"`, `"unknown"`, 1)
	}
	execute(t, d.conn, "INSERT INTO vms VALUES ('retained-vm',?1)", record)
	return path
}
func TestConversionPreservesIdentityAndMaterializesStableNativeBackupState(t *testing.T) {
	source := v5Fixture(t, "5", false)
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(filepath.Dir(source), "converted.db")
	count, err := ConvertV5(context.Background(), source, output)
	if err != nil || count != 1 {
		t.Fatal(count, err)
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("source changed", err)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	d, err := openDatabase(context.Background(), output, true, 2000)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var raw sqlText
	if err := d.conn.QueryRowContext(context.Background(), "SELECT data FROM vms").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var first, second model.Vm
	if err := json.Unmarshal([]byte(raw), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &second); err != nil {
		t.Fatal(err)
	}
	if first.Backup.Revision != second.Backup.Revision || first.Backup.Revision == "" || first.Backup.HasArtifacts() || first.ReservedDisk() != 8196 || first.ID != "retained-vm" {
		t.Fatal(first)
	}
	original, err := openDatabase(context.Background(), source, true, 2000)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	var previous sqlText
	if err := original.conn.QueryRowContext(context.Background(), "SELECT data FROM vms").Scan(&previous); err != nil {
		t.Fatal(err)
	}
	a, err := model.DecodeJSONValue([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	b, err := model.DecodeJSONValue([]byte(previous))
	if err != nil {
		t.Fatal(err)
	}
	delete(a.(map[string]any), "backup")
	if !reflect.DeepEqual(a, b) {
		t.Fatal(a, b)
	}
	metadata, err := textPairs(context.Background(), d.conn, "SELECT key,value FROM _meta ORDER BY key")
	if err != nil || !reflect.DeepEqual(metadata, []metadataPair{{"host_id", "retained-host"}, {"network_config", "retained-network"}, {"schema_version", "6"}}) {
		t.Fatal(metadata, err)
	}
	if _, err := ConvertV5(context.Background(), source, output); err == nil {
		t.Fatal("existing output overwritten")
	}
}
func TestConversionRefusesIncompatibleRecordsWithoutPublishingOrChangingSource(t *testing.T) {
	for _, tc := range []struct {
		version string
		invalid bool
	}{{"4", false}, {"6", false}, {"5", true}} {
		source := v5Fixture(t, tc.version, tc.invalid)
		before, _ := os.ReadFile(source)
		output := filepath.Join(filepath.Dir(source), "converted.db")
		if _, err := ConvertV5(context.Background(), source, output); err == nil {
			t.Fatal("invalid source accepted")
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatal("output published", err)
		}
		after, _ := os.ReadFile(source)
		if !bytes.Equal(before, after) {
			t.Fatal("source changed")
		}
	}
}
func TestConversionRefusesAnAgentThatStillOwnsTheStateLock(t *testing.T) {
	source := v5Fixture(t, "5", false)
	lock, err := runcmd.LockState(filepath.Join(filepath.Dir(source), "service.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := ConvertV5(context.Background(), source, filepath.Join(filepath.Dir(source), "converted.db")); err == nil {
		t.Fatal("live state lock ignored")
	}
}
func TestConversionRefusesIdentityBackupAndSchemaMismatch(t *testing.T) {
	for _, mutation := range []string{
		"UPDATE _meta SET value='another-host' WHERE key='host_id'",
		"UPDATE vms SET id='wrong-id'",
		"UPDATE vms SET data=json_set(data,'$.backup',json('{\"revision\":\"unexpected\"}'))",
		"CREATE TABLE extra (data TEXT)",
		"ALTER TABLE vms ADD COLUMN unexpected TEXT",
	} {
		source := v5Fixture(t, "5", false)
		d, err := openDatabase(context.Background(), source, false, 5000)
		if err != nil {
			t.Fatal(err)
		}
		execute(t, d.conn, mutation)
		d.Close()
		output := filepath.Join(filepath.Dir(source), "converted.db")
		if _, err := ConvertV5(context.Background(), source, output); err == nil {
			t.Fatal(mutation)
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatal("output published", err)
		}
	}
}
func TestConversionDoesNotFollowOrReplaceDestinationSymlink(t *testing.T) {
	source := v5Fixture(t, "5", false)
	dir := filepath.Dir(source)
	output := filepath.Join(dir, "converted.db")
	target := filepath.Join(dir, "absent")
	if err := os.Symlink(target, output); err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertV5(context.Background(), source, output); err == nil {
		t.Fatal("destination symlink accepted")
	}
	if got, err := os.Readlink(output); err != nil || got != target {
		t.Fatal(got, err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("link target created", err)
	}
}

func TestOfflineConnectionsPreserveRusqliteBusyHandling(t *testing.T) {
	for _, operation := range []string{"convert", "identity"} {
		t.Run(operation, func(t *testing.T) {
			source := v5Fixture(t, "5", false)
			if operation == "identity" {
				db, err := openDatabase(context.Background(), source, false, 5000)
				if err != nil {
					t.Fatal(err)
				}
				execute(t, db.conn, "DELETE FROM vms; UPDATE _meta SET value='6' WHERE key='schema_version'")
				db.Close()
			}
			blocker, err := openDatabase(context.Background(), source, false, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Close()
			execute(t, blocker.conn, "BEGIN EXCLUSIVE")
			defer blocker.conn.ExecContext(context.Background(), "ROLLBACK")
			output := filepath.Join(filepath.Dir(source), "output.db")
			done := make(chan error, 1)
			started := make(chan struct{})
			go func() {
				close(started)
				var err error
				if operation == "convert" {
					_, err = ConvertV5(context.Background(), source, output)
				} else {
					_, err = ResolveHostID(context.Background(), source, nil)
				}
				done <- err
			}()
			<-started
			select {
			case err := <-done:
				t.Fatal("connection did not wait for the short external lock", err)
			case <-time.After(100 * time.Millisecond):
			}
			execute(t, blocker.conn, "ROLLBACK")
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("connection did not recover after lock release")
			}
		})
	}
}
