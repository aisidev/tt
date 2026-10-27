package ctl

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/model"
	_ "modernc.org/sqlite"
)

const schemaVersion = 9

type queryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type textColumn string

func (s *textColumn) Scan(value any) error {
	text, ok := value.(string)
	if !ok || !utf8.ValidString(text) {
		return fmt.Errorf("expected UTF-8 SQLite TEXT")
	}
	*s = textColumn(text)
	return nil
}

// DB is serialized by the controller's short database mutex. One physical
// connection retains WAL/FULL/foreign_keys settings for every operation.
type DB struct {
	pool   *sql.DB
	conn   *sql.Conn
	q      queryer
	stderr io.Writer
}

func OpenDB(ctx context.Context, path string) (*DB, error) {
	values := url.Values{"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(ON)"}}
	dsn := path + "?" + values.Encode()
	if path != ":memory:" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		values.Set("mode", "rwc")
		dsn = (&url.URL{Scheme: "file", Path: absolute, RawQuery: values.Encode()}).String()
	}
	pool, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open DB: %w", err)
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	conn, err := pool.Conn(ctx)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("open DB: %w", err)
	}
	db := &DB{pool: pool, conn: conn, q: conn, stderr: os.Stderr}
	if err := db.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
func (d *DB) Close() error { return errors.Join(d.conn.Close(), d.pool.Close()) }
func (d *DB) version(ctx context.Context) (uint32, error) {
	var value textColumn
	err := d.q.QueryRowContext(ctx, "SELECT value FROM _meta WHERE key='schema_version'").Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	version, err := strconv.ParseUint(strings.TrimPrefix(string(value), "+"), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid schema_version: %s", value)
	}
	return uint32(version), nil
}
func (d *DB) setVersion(ctx context.Context, version uint32) error {
	_, err := d.q.ExecContext(ctx, "INSERT OR REPLACE INTO _meta (key,value) VALUES ('schema_version',?1)", strconv.FormatUint(uint64(version), 10))
	if err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	return nil
}
func (d *DB) transaction(ctx context.Context, label string, work func(*DB) error) error {
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	defer tx.Rollback()
	inside := *d
	inside.q = tx
	if err := work(&inside); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s: %w", label, err)
	}
	return nil
}
func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.q.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS _meta (key TEXT PRIMARY KEY,value TEXT NOT NULL);"); err != nil {
		return fmt.Errorf("create meta table: %w", err)
	}
	current, err := d.version(ctx)
	if err != nil {
		return err
	}
	if current > schemaVersion {
		return fmt.Errorf("database schema v%d is newer than this binary (v%d); upgrade TTstack first", current, schemaVersion)
	}
	if current < 1 {
		_, err := d.q.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS hosts (id TEXT PRIMARY KEY,data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS envs (id TEXT PRIMARY KEY,data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS vms (id TEXT PRIMARY KEY,env_id TEXT NOT NULL,host_id TEXT NOT NULL,data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS idx_vms_env ON vms(env_id);
CREATE INDEX IF NOT EXISTS idx_vms_host ON vms(host_id);`)
		if err != nil {
			return fmt.Errorf("migration v1: %w", err)
		}
	}
	if current < 5 {
		if err := d.transaction(ctx, "backup migration", func(tx *DB) error {
			rows, err := tx.q.QueryContext(ctx, "SELECT id,data FROM vms")
			if err != nil {
				return err
			}
			type record struct{ id, data string }
			records := []record{}
			for rows.Next() {
				var id, data textColumn
				if err := rows.Scan(&id, &data); err != nil {
					rows.Close()
					return err
				}
				records = append(records, record{string(id), string(data)})
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			for _, record := range records {
				value, err := model.DecodeJSONValue([]byte(record.data))
				if err != nil {
					return fmt.Errorf("VM backup migration: %w", err)
				}
				object, ok := value.(map[string]any)
				if !ok {
					return fmt.Errorf("invalid VM record during backup migration")
				}
				if _, present := object["backup"]; !present {
					object["backup"] = model.NewBackupState()
				}
				data, err := model.SerdeJSON(object)
				if err != nil {
					return err
				}
				if _, err := tx.q.ExecContext(ctx, "UPDATE vms SET data=?1 WHERE id=?2", string(data), record.id); err != nil {
					return err
				}
			}
			return tx.setVersion(ctx, 5)
		}); err != nil {
			return err
		}
	}
	if current < 6 {
		if err := d.transaction(ctx, "command and observation migration", func(tx *DB) error {
			if _, err := tx.q.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS observation_clock(id INTEGER PRIMARY KEY CHECK(id=1),revision INTEGER NOT NULL);
INSERT OR IGNORE INTO observation_clock VALUES(1,0);
CREATE TABLE IF NOT EXISTS command_fences(subject TEXT PRIMARY KEY,token INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS command_receipts(subject TEXT NOT NULL,key TEXT NOT NULL,fingerprint TEXT NOT NULL,digest TEXT NOT NULL,status INTEGER NOT NULL,response BLOB NOT NULL,PRIMARY KEY(subject,key));`); err != nil {
				return err
			}
			return tx.setVersion(ctx, 6)
		}); err != nil {
			return err
		}
	}
	if current < 7 {
		if err := d.transaction(ctx, "volume catalog migration", func(tx *DB) error {
			if _, err := tx.q.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS volumes(id TEXT PRIMARY KEY,data TEXT NOT NULL); CREATE TABLE IF NOT EXISTS volume_snapshots(id TEXT PRIMARY KEY,data TEXT NOT NULL);`); err != nil {
				return err
			}
			return tx.setVersion(ctx, 7)
		}); err != nil {
			return err
		}
	}
	if current < 8 {
		if err := d.transaction(ctx, "recovery manifest migration", func(tx *DB) error {
			if _, err := tx.q.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS recovery_points(id TEXT PRIMARY KEY,data TEXT NOT NULL); CREATE TABLE IF NOT EXISTS recovery_restores(id TEXT PRIMARY KEY,data TEXT NOT NULL);`); err != nil {
				return err
			}
			return tx.setVersion(ctx, 8)
		}); err != nil {
			return err
		}
	}
	if current < 9 {
		if err := d.transaction(ctx, "image catalog migration", func(tx *DB) error {
			if _, err := tx.q.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS image_catalog(id TEXT PRIMARY KEY,data TEXT NOT NULL)`); err != nil {
				return err
			}
			return tx.setVersion(ctx, schemaVersion)
		}); err != nil {
			return err
		}
	}
	if current < schemaVersion {
		fmt.Fprintf(d.stderr, "database migrated: v%d → v%d\n", current, schemaVersion)
	}
	return nil
}
func putRecord(ctx context.Context, q queryer, query, label string, value any, identity ...any) error {
	data, err := model.SerdeJSON(value)
	if err != nil {
		return fmt.Errorf("serialize %s: %w", label, err)
	}
	args := append(identity, string(data))
	if _, err := q.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("put %s: %w", label, err)
	}
	return nil
}
func queryOne[T any](ctx context.Context, q queryer, query string, args ...any) (*T, error) {
	var data textColumn
	err := q.QueryRowContext(ctx, query, args...).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	var value T
	if err := json.Unmarshal([]byte(data), &value); err != nil {
		return nil, fmt.Errorf("deserialize: %w", err)
	}
	return &value, nil
}
func queryAll[T any](ctx context.Context, q queryer, query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	result := []T{}
	for rows.Next() {
		var data textColumn
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("read row: %w", err)
		}
		var value T
		if err := json.Unmarshal([]byte(data), &value); err != nil {
			return nil, fmt.Errorf("deserialize: %w", err)
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

// recoveryRows returns the decodable rows plus the ids of the unreadable ones.
// Callers must account for the corrupt ids explicitly; silently dropping them
// would leave those records invisible to every later pass.
func recoveryRows[T any](ctx context.Context, d *DB, table string) ([]T, []string, error) {
	rows, err := d.q.QueryContext(ctx, "SELECT id,data FROM "+table+" ORDER BY id")
	if err != nil {
		return nil, nil, fmt.Errorf("prepare recovery: %w", err)
	}
	defer rows.Close()
	result := []T{}
	corrupt := []string{}
	for rows.Next() {
		var id, data textColumn
		if err := rows.Scan(&id, &data); err != nil {
			return nil, nil, fmt.Errorf("recovery row: %w", err)
		}
		var value T
		if err := json.Unmarshal([]byte(data), &value); err != nil {
			fmt.Fprintf(d.stderr, "[ctl] unreadable %s record %s; replaced by an offline marker\n", table, id)
			corrupt = append(corrupt, string(id))
		} else {
			result = append(result, value)
		}
	}
	return result, corrupt, rows.Err()
}
func (d *DB) PutHost(ctx context.Context, host *model.Host) error {
	return putRecord(ctx, d.q, "INSERT OR REPLACE INTO hosts (id,data) VALUES (?1,?2)", "host", host, host.ID)
}
func (d *DB) GetHost(ctx context.Context, id string) (*model.Host, error) {
	return queryOne[model.Host](ctx, d.q, "SELECT data FROM hosts WHERE id=?1", id)
}

// All fleet listings order by id: INSERT OR REPLACE rewrites move rows to the
// end of the table, and deterministic input order keeps scheduling tie-breaks
// and rebuilt wire arrays stable.
func (d *DB) ListHosts(ctx context.Context) ([]model.Host, error) {
	return queryAll[model.Host](ctx, d.q, "SELECT data FROM hosts ORDER BY id")
}
func (d *DB) RemoveHost(ctx context.Context, id string) error {
	_, err := d.q.ExecContext(ctx, "DELETE FROM hosts WHERE id=?1", id)
	if err != nil {
		return fmt.Errorf("remove host: %w", err)
	}
	return nil
}
func (d *DB) PutEnv(ctx context.Context, env *model.Env) error {
	return putRecord(ctx, d.q, "INSERT OR REPLACE INTO envs (id,data) VALUES (?1,?2)", "env", env, env.ID)
}
func (d *DB) GetEnv(ctx context.Context, id string) (*model.Env, error) {
	return queryOne[model.Env](ctx, d.q, "SELECT data FROM envs WHERE id=?1", id)
}
func (d *DB) ListEnvs(ctx context.Context) ([]model.Env, error) {
	return queryAll[model.Env](ctx, d.q, "SELECT data FROM envs")
}
func (d *DB) RemoveEnv(ctx context.Context, id string) error {
	_, err := d.q.ExecContext(ctx, "DELETE FROM envs WHERE id=?1", id)
	if err != nil {
		return fmt.Errorf("remove env: %w", err)
	}
	return nil
}
func (d *DB) PutVM(ctx context.Context, vm *model.Vm) error {
	return putRecord(ctx, d.q, "INSERT OR REPLACE INTO vms (id,env_id,host_id,data) VALUES (?1,?2,?3,?4)", "VM", vm, vm.ID, vm.EnvID, vm.HostID)
}
func (d *DB) GetVM(ctx context.Context, id string) (*model.Vm, error) {
	return queryOne[model.Vm](ctx, d.q, "SELECT data FROM vms WHERE id=?1", id)
}
func (d *DB) RemoveVM(ctx context.Context, id string) error {
	_, err := d.q.ExecContext(ctx, "DELETE FROM vms WHERE id=?1", id)
	if err != nil {
		return fmt.Errorf("remove VM: %w", err)
	}
	return nil
}
func (d *DB) VMsByEnv(ctx context.Context, id string) ([]model.Vm, error) {
	return queryAll[model.Vm](ctx, d.q, "SELECT data FROM vms WHERE env_id=?1 ORDER BY id", id)
}
func (d *DB) VMsByHost(ctx context.Context, id string) ([]model.Vm, error) {
	return queryAll[model.Vm](ctx, d.q, "SELECT data FROM vms WHERE host_id=?1 ORDER BY id", id)
}
func (d *DB) RecoveryHosts(ctx context.Context) ([]model.Host, []string, error) {
	return recoveryRows[model.Host](ctx, d, "hosts")
}
func (d *DB) RecoveryEnvs(ctx context.Context) ([]model.Env, []string, error) {
	return recoveryRows[model.Env](ctx, d, "envs")
}
func (d *DB) count(ctx context.Context, table string) (uint64, error) {
	var count uint64
	err := d.q.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count %s: %w", table, err)
	}
	return count, nil
}
func (d *DB) HostCount(ctx context.Context) (uint64, error) { return d.count(ctx, "hosts") }
func (d *DB) EnvCount(ctx context.Context) (uint64, error)  { return d.count(ctx, "envs") }
func (d *DB) VMCount(ctx context.Context) (uint64, error)   { return d.count(ctx, "vms") }

// PutEnvironment commits the whole plan — environment, VMs and the host
// reservations — atomically, so a mid-write failure cannot park an
// environment in Creating with half-deducted hosts.
func (d *DB) PutEnvironment(ctx context.Context, env *model.Env, vms []model.Vm, hosts []model.Host) error {
	return d.transaction(ctx, "environment plan", func(tx *DB) error {
		if err := tx.PutEnv(ctx, env); err != nil {
			return err
		}
		for i := range vms {
			if err := tx.PutVM(ctx, &vms[i]); err != nil {
				return err
			}
		}
		for i := range hosts {
			if err := tx.PutHost(ctx, &hosts[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// RemoveVMRelease deletes the VM row and returns its reservations to the host
// in the same transaction. A missing or unreadable host row does not block
// the deletion: accounting repair is the refresh loop's job.
func (d *DB) RemoveVMRelease(ctx context.Context, vm model.Vm) error {
	return d.transaction(ctx, "VM removal", func(tx *DB) error {
		if err := tx.RemoveVM(ctx, vm.ID); err != nil {
			return err
		}
		host, err := tx.GetHost(ctx, vm.HostID)
		if err != nil || host == nil {
			return nil
		}
		host.Resource.Release(vm)
		return tx.PutHost(ctx, host)
	})
}

// PutVMObservation commits a VM and the change in its host disk reservation
// together, including backup observations and pending resource updates.
func (d *DB) PutVMObservation(ctx context.Context, vm *model.Vm, previousDisk uint32) error {
	return d.transaction(ctx, "VM observation", func(tx *DB) error {
		if err := tx.PutVM(ctx, vm); err != nil {
			return err
		}
		host, err := tx.GetHost(ctx, vm.HostID)
		if err != nil {
			return err
		}
		if host != nil {
			host.Resource.DiskUsed = model.SatAdd(model.SatSub(host.Resource.DiskUsed, previousDisk), vm.ReservedDisk())
			return tx.PutHost(ctx, host)
		}
		return nil
	})
}
func (d *DB) DetachHost(ctx context.Context, id string) ([]model.Vm, error) {
	vms, err := d.VMsByHost(ctx, id)
	if err != nil {
		return nil, err
	}
	removed := map[string]bool{}
	for _, vm := range vms {
		removed[vm.ID] = true
	}
	if err := d.transaction(ctx, "detach", func(tx *DB) error {
		for _, vm := range vms {
			env, err := tx.GetEnv(ctx, vm.EnvID)
			if err != nil {
				return err
			}
			if env != nil {
				retained := model.List[string]{}
				for _, id := range env.VmIds {
					if !removed[id] {
						retained = append(retained, id)
					}
				}
				env.VmIds = retained
				env.State = model.EnvFailed
				message := "host " + id + " detached; guests may still exist on that host"
				env.Error = &message
				if err := tx.PutEnv(ctx, env); err != nil {
					return err
				}
			}
		}
		if _, err := tx.q.ExecContext(ctx, "DELETE FROM vms WHERE host_id=?1", id); err != nil {
			return fmt.Errorf("detach VM tracking: %w", err)
		}
		return tx.RemoveHost(ctx, id)
	}); err != nil {
		return nil, err
	}
	return vms, nil
}
func (d *DB) FleetStatus(ctx context.Context) (api.FleetStatus, error) {
	hosts, err := d.ListHosts(ctx)
	if err != nil {
		return api.FleetStatus{}, err
	}
	result := api.FleetStatus{Hosts: uint32(len(hosts))}
	for _, host := range hosts {
		if host.State != model.HostOnline {
			continue
		}
		result.HostsOnline++
		result.CPUTotal += host.Resource.CPUTotal
		result.CPUUsed += host.Resource.CPUUsed
		result.MemTotal += host.Resource.MemTotal
		result.MemUsed += host.Resource.MemUsed
		result.DiskTotal += host.Resource.DiskTotal
		result.DiskUsed += host.Resource.DiskUsed
	}
	count, err := d.VMCount(ctx)
	if err != nil {
		return api.FleetStatus{}, err
	}
	result.TotalVMs = uint32(count)
	count, err = d.EnvCount(ctx)
	if err != nil {
		return api.FleetStatus{}, err
	}
	result.TotalEnvs = uint32(count)
	return result, nil
}
