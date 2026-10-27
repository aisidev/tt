package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
)

type metadataPair struct{ key, value string }

func textPairs(ctx context.Context, q sqlQuery, query string) ([]metadataPair, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pairs := []metadataPair{}
	for rows.Next() {
		var key, value sqlText
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		pairs = append(pairs, metadataPair{string(key), string(value)})
	}
	return pairs, rows.Err()
}

// ConvertV5 creates a distinct v6 database under the source agent's state lock.
// The input stays read-only, unknown JSON fields survive, and publication never
// replaces an existing destination, including a concurrently created one.
func ConvertV5(ctx context.Context, source, output string) (int, error) {
	absolute, err := filepath.Abs(source)
	if err != nil {
		return 0, fmt.Errorf("resolve source database: %w", err)
	}
	source, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return 0, fmt.Errorf("resolve source database: %w", err)
	}
	info, err := os.Stat(source)
	if err != nil {
		return 0, fmt.Errorf("resolve source database: %w", err)
	}
	_, outputErr := os.Stat(output)
	if outputErr == nil || !info.Mode().IsRegular() {
		return 0, fmt.Errorf("source must be a file and output must not exist")
	}
	lock, err := runcmd.LockState(filepath.Join(filepath.Dir(source), "service.lock"))
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	input, err := openDatabase(ctx, source, true, 5000)
	if err != nil {
		return 0, fmt.Errorf("open source read-only: %w", err)
	}
	defer input.Close()
	snapshot, err := input.conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("read source snapshot: %w", err)
	}
	defer snapshot.Rollback()
	var check sqlText
	if err := snapshot.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
		return 0, err
	}
	if check != "ok" {
		return 0, fmt.Errorf("source integrity check failed")
	}
	objects, err := textPairs(ctx, snapshot, "SELECT type,name FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*' ORDER BY name")
	if err != nil {
		return 0, err
	}
	if !reflect.DeepEqual(objects, []metadataPair{{"table", "_meta"}, {"table", "vms"}}) {
		return 0, fmt.Errorf("source is not the supported native v5 schema")
	}
	for _, table := range []struct {
		name    string
		columns []string
	}{{"_meta", []string{"key", "value"}}, {"vms", []string{"id", "data"}}} {
		rows, err := snapshot.QueryContext(ctx, "SELECT * FROM "+table.name+" LIMIT 0")
		if err != nil {
			return 0, err
		}
		columns, err := rows.Columns()
		rows.Close()
		if err != nil {
			return 0, err
		}
		if !reflect.DeepEqual(columns, table.columns) {
			return 0, fmt.Errorf("unexpected source table columns")
		}
	}
	metadata, err := textPairs(ctx, snapshot, "SELECT key,value FROM _meta ORDER BY key")
	if err != nil {
		return 0, err
	}
	version, host := "", ""
	for _, pair := range metadata {
		if pair.key == "schema_version" {
			version = pair.value
		}
		if pair.key == "host_id" {
			host = pair.value
		}
	}
	if version != "5" {
		return 0, fmt.Errorf("conversion requires agent schema v5")
	}
	if host == "" {
		return 0, fmt.Errorf("source has no stable host identity")
	}
	records, err := textPairs(ctx, snapshot, "SELECT id,data FROM vms ORDER BY id")
	if err != nil {
		return 0, err
	}
	converted := make([]metadataPair, 0, len(records))
	for _, record := range records {
		var vm model.Vm
		if err := json.Unmarshal([]byte(record.value), &vm); err != nil {
			return 0, fmt.Errorf("invalid source VM record: %w", err)
		}
		value, err := model.DecodeJSONValue([]byte(record.value))
		if err != nil {
			return 0, err
		}
		object, ok := value.(map[string]any)
		if !ok {
			return 0, fmt.Errorf("VM must be an object")
		}
		_, backupPresent := object["backup"]
		if vm.ID != record.key || vm.HostID != host || backupPresent {
			return 0, fmt.Errorf("VM identity mismatch or unexpected backup state in v5 source")
		}
		object["backup"] = vm.Backup
		data, err := model.SerdeJSON(object)
		if err != nil {
			return 0, err
		}
		converted = append(converted, metadataPair{record.key, string(data)})
	}
	parent := filepath.Dir(output)
	temporary, err := os.CreateTemp(parent, ".tt-convert-*")
	if err != nil {
		return 0, fmt.Errorf("create private conversion file: %w", err)
	}
	defer temporary.Close()
	defer os.Remove(temporary.Name())
	destination, err := openDatabase(ctx, temporary.Name(), false, 5000)
	if err != nil {
		return 0, err
	}
	if err := writeConverted(ctx, destination, metadata, converted); err != nil {
		destination.Close()
		return 0, err
	}
	if err := destination.Close(); err != nil {
		return 0, err
	}
	if err := temporary.Sync(); err != nil {
		return 0, err
	}
	// Same-directory hard-link publication is atomic and fails on any existing
	// directory entry. It matches persist_noclobber without a check/rename race.
	if err := os.Link(temporary.Name(), output); err != nil {
		return 0, fmt.Errorf("publish new database without overwrite: %w", err)
	}
	directory, err := os.Open(parent)
	if err != nil {
		return 0, err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return 0, err
	}
	return len(converted), nil
}
func writeConverted(ctx context.Context, d *database, metadata, records []metadataPair) error {
	if _, err := d.conn.ExecContext(ctx, "PRAGMA synchronous=FULL;"); err != nil {
		return err
	}
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "CREATE TABLE _meta (key TEXT PRIMARY KEY,value TEXT NOT NULL); CREATE TABLE vms (id TEXT PRIMARY KEY,data TEXT NOT NULL);"); err != nil {
		return err
	}
	for _, pair := range metadata {
		value := pair.value
		if pair.key == "schema_version" {
			value = "6"
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO _meta VALUES (?1,?2)", pair.key, value); err != nil {
			return err
		}
	}
	for _, pair := range records {
		if _, err := tx.ExecContext(ctx, "INSERT INTO vms VALUES (?1,?2)", pair.key, pair.value); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit converted metadata: %w", err)
	}
	var check sqlText
	if err := d.conn.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
		return err
	}
	if check != "ok" {
		return fmt.Errorf("converted database integrity check failed")
	}
	return nil
}
