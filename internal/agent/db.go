package agent

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

	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/internal/engine"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const agentSchemaVersion = 10

// The writer owns one physical connection. Initialization gates schema writes
// before enabling WAL/FULL; snapshots open independent read-only connections.
// Keeping the connection pinned makes connection-local pragmas unambiguous.
type database struct {
	pool   *sql.DB
	conn   *sql.Conn
	stderr io.Writer
}
type sqlQuery interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// rusqlite's String extraction rejects BLOB, INTEGER and NULL values. Scanning
// directly into Go string would silently coerce several of those corrupt rows.
type sqlText string

func (s *sqlText) Scan(value any) error {
	text, ok := value.(string)
	if !ok || !utf8.ValidString(text) {
		return fmt.Errorf("expected UTF-8 SQLite TEXT")
	}
	*s = sqlText(text)
	return nil
}
func openDatabase(ctx context.Context, path string, readOnly bool, timeout int) (*database, error) {
	query := url.Values{"_pragma": {fmt.Sprintf("busy_timeout(%d)", timeout)}}
	var dsn string
	if path == ":memory:" {
		dsn = path + "?" + query.Encode()
	} else {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if readOnly {
			query.Set("mode", "ro")
		} else {
			query.Set("mode", "rwc")
		}
		dsn = (&url.URL{Scheme: "file", Path: absolute, RawQuery: query.Encode()}).String()
	}
	pool, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	conn, err := pool.Conn(ctx)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return &database{pool: pool, conn: conn, stderr: os.Stderr}, nil
}
func (d *database) Close() error { return errors.Join(d.conn.Close(), d.pool.Close()) }
func openAgentDatabase(ctx context.Context, path string) (*database, error) {
	d, err := openDatabase(ctx, path, false, 5000)
	if err != nil {
		return nil, fmt.Errorf("open agent DB: %w", err)
	}
	if err := d.initialize(ctx); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}
func schemaVersion(ctx context.Context, q sqlQuery) (uint32, error) {
	var value sqlText
	err := q.QueryRowContext(ctx, "SELECT value FROM _meta WHERE key='schema_version'").Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("agent database has no schema marker; deployment must supply a native database")
	}
	if err != nil {
		return 0, fmt.Errorf("read agent database format; incompatible state must be handled by the caller's deployment script: %w", err)
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(string(value), "+"), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid agent database schema marker: %w", err)
	}
	return uint32(n), nil
}
func (d *database) initialize(ctx context.Context) error {
	var populated bool
	if err := d.conn.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*')").Scan(&populated); err != nil {
		return fmt.Errorf("inspect agent database format: %w", err)
	}
	if populated {
		current, err := schemaVersion(ctx, d.conn)
		if err != nil {
			return err
		}
		if current == 6 || current == 7 || current == 8 || current == 9 || current == agentSchemaVersion {
			stmt, err := d.conn.PrepareContext(ctx, "SELECT id,data FROM vms")
			if err != nil {
				return fmt.Errorf("validate agent VM table: %w", err)
			}
			stmt.Close()
		}
		if current == 6 {
			tx, err := d.conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err = tx.ExecContext(ctx, command.Schema); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE _meta SET value='7' WHERE key='schema_version'"); err != nil {
				return err
			}
			if err = tx.Commit(); err != nil {
				return err
			}
			current = 7
		}
		if current == 7 {
			tx, err := d.conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err = tx.ExecContext(ctx, diskSchema); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE _meta SET value='8' WHERE key='schema_version'"); err != nil {
				return err
			}
			if err = tx.Commit(); err != nil {
				return err
			}
			current = 8
		}
		if current == 8 {
			tx, e := d.conn.BeginTx(ctx, nil)
			if e != nil {
				return e
			}
			defer tx.Rollback()
			if _, e = tx.ExecContext(ctx, checkpointSchema); e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, "UPDATE _meta SET value='9' WHERE key='schema_version'"); e != nil {
				return e
			}
			if e = tx.Commit(); e != nil {
				return e
			}
			current = 9
		}
		if current == 9 {
			tx, e := d.conn.BeginTx(ctx, nil)
			if e != nil {
				return e
			}
			defer tx.Rollback()
			if _, e = tx.ExecContext(ctx, archiveSchema); e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, "UPDATE _meta SET value='10' WHERE key='schema_version'"); e != nil {
				return e
			}
			if e = tx.Commit(); e != nil {
				return e
			}
			current = 10
		}
		if current != agentSchemaVersion {
			return fmt.Errorf("unsupported agent DB schema v%d; expected v%d; database migration or cleanup belongs to the caller's deployment script", current, agentSchemaVersion)
		}
		stmt, err := d.conn.PrepareContext(ctx, "SELECT id, data FROM vms")
		if err != nil {
			return fmt.Errorf("validate agent VM table: %w", err)
		}
		stmt.Close()
	} else {
		tx, err := d.conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("initialize agent database: %w", err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "CREATE TABLE _meta (key TEXT PRIMARY KEY,value TEXT NOT NULL); CREATE TABLE vms (id TEXT PRIMARY KEY,data TEXT NOT NULL);"+command.Schema+diskSchema+checkpointSchema+archiveSchema); err != nil {
			return fmt.Errorf("create native agent schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO _meta (key,value) VALUES ('schema_version',?1)", strconv.Itoa(agentSchemaVersion)); err != nil {
			return fmt.Errorf("record agent database format: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit agent database initialization: %w", err)
		}
	}
	if _, err := d.conn.ExecContext(ctx, "PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;"); err != nil {
		return fmt.Errorf("configure agent database: %w", err)
	}
	return nil
}
func ResolveHostID(ctx context.Context, path string, requested *string) (string, error) {
	d, err := openAgentDatabase(ctx, path)
	if err != nil {
		return "", err
	}
	defer d.Close()
	if requested != nil {
		if err := model.ValidateName(*requested, "host_id"); err != nil {
			return "", err
		}
		if _, err := d.conn.ExecContext(ctx, "INSERT OR REPLACE INTO _meta (key,value) VALUES ('host_id',?1)", *requested); err != nil {
			return "", fmt.Errorf("persist CLI host_id: %w", err)
		}
		return *requested, nil
	}
	var stored sqlText
	err = d.conn.QueryRowContext(ctx, "SELECT value FROM _meta WHERE key='host_id'").Scan(&stored)
	if err == nil {
		return string(stored), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id := uuid.NewString()[:8]
	if _, err := d.conn.ExecContext(ctx, "INSERT OR REPLACE INTO _meta (key,value) VALUES ('host_id',?1)", id); err != nil {
		return "", fmt.Errorf("persist generated host_id: %w", err)
	}
	return id, nil
}
func saveVM(ctx context.Context, q sqlQuery, vm *model.Vm) error {
	data, err := model.SerdeJSON(vm)
	if err != nil {
		return fmt.Errorf("serialize VM: %w", err)
	}
	if _, err := q.ExecContext(ctx, "INSERT OR REPLACE INTO vms (id,data) VALUES (?1,?2)", vm.ID, string(data)); err != nil {
		return fmt.Errorf("save VM: %w", err)
	}
	return nil
}
func loadVM(ctx context.Context, q sqlQuery, id string) (*model.Vm, error) {
	var data sqlText
	err := q.QueryRowContext(ctx, "SELECT data FROM vms WHERE id=?1", id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query VM: %w", err)
	}
	var vm model.Vm
	if err := json.Unmarshal([]byte(data), &vm); err != nil {
		return nil, fmt.Errorf("deserialize VM: %w", err)
	}
	return &vm, nil
}
func loadAllVMs(ctx context.Context, q sqlQuery) ([]model.Vm, error) {
	rows, err := q.QueryContext(ctx, "SELECT data FROM vms")
	if err != nil {
		return nil, fmt.Errorf("query all VMs: %w", err)
	}
	defer rows.Close()
	vms := []model.Vm{}
	for rows.Next() {
		var data sqlText
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("read row: %w", err)
		}
		var vm model.Vm
		if err := json.Unmarshal([]byte(data), &vm); err != nil {
			return nil, fmt.Errorf("deserialize VM: %w", err)
		}
		vms = append(vms, vm)
	}
	return vms, rows.Err()
}
func loadRecoverableVMs(ctx context.Context, q sqlQuery, stderr io.Writer) ([]model.Vm, bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT id,data FROM vms")
	if err != nil {
		return nil, false, fmt.Errorf("read recovery records: %w", err)
	}
	defer rows.Close()
	vms := []model.Vm{}
	corrupt := false
	for rows.Next() {
		var id, data sqlText
		if err := rows.Scan(&id, &data); err != nil {
			return nil, false, fmt.Errorf("recovery row: %w", err)
		}
		var vm model.Vm
		if err := json.Unmarshal([]byte(data), &vm); err == nil && vm.ID == string(id) {
			vms = append(vms, vm)
		} else {
			corrupt = true
			fmt.Fprintf(stderr, "[agent] unreadable VM record %s; retained, admission disabled\n", id)
		}
	}
	return vms, corrupt, rows.Err()
}
func deleteVM(ctx context.Context, q sqlQuery, id string) error {
	_, err := q.ExecContext(ctx, "DELETE FROM vms WHERE id=?1", id)
	if err != nil {
		return fmt.Errorf("delete VM: %w", err)
	}
	return nil
}
func ReadVMs(ctx context.Context, path string) ([]model.Vm, error) {
	d, err := openDatabase(ctx, path, true, 2000)
	if err != nil {
		return nil, fmt.Errorf("open agent snapshot: %w", err)
	}
	defer d.Close()
	return loadAllVMs(ctx, d.conn)
}
func ReadVM(ctx context.Context, path, id string) (*model.Vm, error) {
	d, err := openDatabase(ctx, path, true, 2000)
	if err != nil {
		return nil, fmt.Errorf("open VM snapshot: %w", err)
	}
	defer d.Close()
	return loadVM(ctx, d.conn, id)
}
func ReadRecoverySnapshot(ctx context.Context, path string, stderr io.Writer) ([]model.Vm, bool, error) {
	d, err := openDatabase(ctx, path, true, 2000)
	if err != nil {
		return nil, false, fmt.Errorf("open agent recovery snapshot: %w", err)
	}
	defer d.Close()
	return loadRecoverableVMs(ctx, d.conn, stderr)
}
func ResourcesFor(totals model.Resource, vms []model.Vm) model.Resource {
	r := model.Resource{CPUTotal: totals.CPUTotal, MemTotal: totals.MemTotal, DiskTotal: totals.DiskTotal}
	for _, vm := range vms {
		r.Account(vm)
	}
	return r
}
func (d *database) bindContainerRuntime(ctx context.Context, requested *engine.ContainerRuntime, detect func() *engine.ContainerRuntime) (*engine.ContainerRuntime, error) {
	var stored sqlText
	err := d.conn.QueryRowContext(ctx, "SELECT value FROM _meta WHERE key='container_runtime'").Scan(&stored)
	var bound *engine.ContainerRuntime
	if err == nil {
		value, err := engine.ParseContainerRuntime(string(stored))
		if err != nil {
			return nil, err
		}
		bound = &value
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read container runtime binding: %w", err)
	}
	vms, corrupt, err := loadRecoverableVMs(ctx, d.conn, d.stderr)
	if err != nil {
		return nil, err
	}
	containers := false
	for _, vm := range vms {
		containers = containers || vm.Engine == model.Docker
	}
	if bound == nil && containers {
		return nil, fmt.Errorf("agent database has container records but no runtime binding; inspect the database before starting")
	}
	occupied := corrupt || containers
	var selected *engine.ContainerRuntime
	switch {
	case bound == nil && requested != nil && corrupt:
		return nil, fmt.Errorf("cannot select a container runtime while unreadable records remain")
	case bound != nil && requested != nil && *bound != *requested && occupied:
		return nil, fmt.Errorf("cannot change the container runtime while container or unreadable records remain; retain %s and drain them first", bound.Command())
	case requested != nil:
		selected = requested
	case bound != nil:
		selected = bound
	case occupied:
		selected = nil
	default:
		selected = detect()
	}
	if selected != nil {
		if _, err := d.conn.ExecContext(ctx, "INSERT OR REPLACE INTO _meta (key,value) VALUES ('container_runtime',?1)", selected.Command()); err != nil {
			return nil, fmt.Errorf("persist container runtime binding: %w", err)
		}
	}
	return selected, nil
}
