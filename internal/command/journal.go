// Package command persists command identity and receiver-side fencing before
// dispatch. It retains receipts and watermarks after resource deletion.
package command

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"

	"github.com/aisidev/tt/api"
)

type Journal struct {
	Authority *Authority
	Conn      *sql.Conn
	Mu        *sync.Mutex
	busyMu    sync.Mutex
	busy      map[string]bool
}

func New(conn *sql.Conn, mu *sync.Mutex) *Journal {
	return &Journal{Conn: conn, Mu: mu, busy: map[string]bool{}}
}

var errFingerprint = errors.New("fingerprint conflict")

type contextKey struct{}

func FromContext(ctx context.Context) (Envelope, bool) {
	value, ok := ctx.Value(contextKey{}).(Envelope)
	return value, ok
}
func CopyContext(parent, source context.Context) context.Context {
	if value, ok := FromContext(source); ok {
		return context.WithValue(parent, contextKey{}, value)
	}
	return parent
}
func (s *Journal) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.Conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

const Schema = `CREATE TABLE IF NOT EXISTS command_fences(subject TEXT PRIMARY KEY,token INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS command_receipts(subject TEXT NOT NULL,key TEXT NOT NULL,fingerprint TEXT NOT NULL,digest TEXT NOT NULL,status INTEGER NOT NULL,response BLOB NOT NULL,PRIMARY KEY(subject,key));`

type Envelope struct {
	Authority   string `json:"authority,omitempty"`
	Epoch       uint64 `json:"epoch,omitempty"`
	Subject     string `json:"subject"`
	Group       string `json:"group,omitempty"`
	Generation  uint64 `json:"generation"`
	Token       uint64 `json:"token"`
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
}

func commandError(w http.ResponseWriter, status int, class, code string) {
	api.WriteJSON(w, status, map[string]any{"ok": false, "failure": api.BoundaryError{Class: class, Code: code, Message: "command admission failed"}})
}

func (s *Journal) Wrap(next http.Handler, supports func(*http.Request) bool, resolve func(*http.Request, []byte) (string, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get("X-Compute-Command")
		if r.Method != "GET" && r.Method != "HEAD" && s.Authority != nil && !supports(r) {
			commandError(w, 403, "denied", "unregistered_mutation")
			return
		}
		if r.Method == "GET" || r.Method == "HEAD" || !supports(r) {
			next.ServeHTTP(w, r)
			return
		}

		limit := api.JSONBodyLimit
		data, err := io.ReadAll(io.LimitReader(r.Body, int64(limit+1)))
		if raw == "" && len(data) > limit {
			api.WriteText(w, 413, "Failed to buffer the request body: length limit exceeded")
			return
		}
		if err != nil || len(data) > limit {
			commandError(w, 400, "terminal", "size_exceeded")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		subject, err := resolve(r, data)
		if err != nil {
			commandError(w, 503, "unknown", "subject_unavailable")
			return
		}
		if subject == "" && s.Authority != nil {
			commandError(w, 400, "terminal", "missing_identity")
			return
		}
		if subject == "" {
			next.ServeHTTP(w, r)
			return
		}
		body := data
		var cmd Envelope
		if raw != "" {
			if json.Unmarshal([]byte(raw), &cmd) != nil || cmd.Subject != subject || cmd.Generation == 0 || cmd.Token == 0 || cmd.Token > 1<<63-1 || cmd.Key == "" || len(cmd.Key) > 200 || len(cmd.Fingerprint) != 64 || !strings.HasSuffix(subject, "-g"+strconv.FormatUint(cmd.Generation, 10)) {
				commandError(w, 400, "terminal", "invalid_identity")
				return
			}
		}
		if err := s.validateAuthority(cmd, r.URL.Path == "/api/control/fence" || r.URL.Path == "/api/control/resume"); err != nil {
			var typed *api.BoundaryError
			if errors.As(err, &typed) {
				commandError(w, typed.Status(), typed.Class, typed.Code)
			} else {
				commandError(w, 503, "unknown", "authority_unavailable")
			}
			return
		}
		if s.Authority != nil && (r.Method == http.MethodDelete || strings.HasSuffix(r.URL.Path, "/import/abort")) && !s.Authority.Active("gc") {
			commandError(w, 409, "state", "gc_fenced")
			return
		}
		s.busyMu.Lock()
		if s.busy == nil {
			s.busy = map[string]bool{}
		}
		if s.busy[subject] {
			s.busyMu.Unlock()
			commandError(w, 409, "state", "command_busy")
			return
		}
		s.busy[subject] = true
		s.busyMu.Unlock()
		defer func() { s.busyMu.Lock(); delete(s.busy, subject); s.busyMu.Unlock() }()
		sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.EscapedPath()+"\n"), body...))
		digest := hex.EncodeToString(sum[:])
		s.Mu.Lock()
		var token int64
		err = s.Conn.QueryRowContext(r.Context(), "SELECT COALESCE((SELECT token FROM command_fences WHERE subject=?1),0)", subject).Scan(&token)
		if err != nil {
			s.Mu.Unlock()
			commandError(w, 503, "unknown", "persistence_unavailable")
			return
		}
		if (raw == "" && token > 0) || (raw != "" && cmd.Token < uint64(token)) {
			s.Mu.Unlock()
			commandError(w, 409, "state", "stale_fence")
			return
		}
		if raw == "" {
			s.Mu.Unlock()
			next.ServeHTTP(w, r)
			return
		}
		var oldFingerprint, oldDigest string
		var status int
		var response []byte
		var present bool
		err = s.transaction(r.Context(), func(q *sql.Tx) error {
			rows, e := q.QueryContext(r.Context(), "SELECT fingerprint,digest,status,response FROM command_receipts WHERE subject=?1 AND key=?2", subject, cmd.Key)
			if e != nil {
				return e
			}
			if rows.Next() {
				present = true
				e = rows.Scan(&oldFingerprint, &oldDigest, &status, &response)
			}
			if e == nil {
				e = rows.Err()
			}
			rows.Close()
			if e != nil {
				return e
			}
			if present && (oldFingerprint != cmd.Fingerprint || oldDigest != digest) {
				return errFingerprint
			}
			if _, e = q.ExecContext(r.Context(), "INSERT INTO command_fences(subject,token) VALUES(?1,?2) ON CONFLICT(subject) DO UPDATE SET token=excluded.token", subject, cmd.Token); e != nil {
				return e
			}
			if present {
				return nil
			}
			_, e = q.ExecContext(r.Context(), "INSERT INTO command_receipts(subject,key,fingerprint,digest,status,response) VALUES(?1,?2,?3,?4,0,X'')", subject, cmd.Key, cmd.Fingerprint, digest)
			return e
		})
		s.Mu.Unlock()
		if errors.Is(err, errFingerprint) {
			commandError(w, 400, "terminal", "fingerprint_conflict")
			return
		}
		if err != nil {
			commandError(w, 503, "unknown", "persistence_unavailable")
			return
		}
		if present && (oldFingerprint != cmd.Fingerprint || oldDigest != digest) {
			commandError(w, 400, "terminal", "fingerprint_conflict")
			return
		}
		if present && status > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(response)
			return
		}
		// A pending receipt is replayed after a crash. Environment identity remains
		// stable, and lifecycle handlers retain their own serialization and intent.
		result := httptest.NewRecorder()
		next.ServeHTTP(result, r.WithContext(context.WithValue(r.Context(), contextKey{}, cmd)))
		// Only successful acceptance is cached. A busy/failed attempt is retried
		// with the same identity, never promoted to a false durable success.
		if result.Code < 400 {
			s.Mu.Lock()
			_, err = s.Conn.ExecContext(context.WithoutCancel(r.Context()), "UPDATE command_receipts SET status=?1,response=?2 WHERE subject=?3 AND key=?4", result.Code, result.Body.String(), subject, cmd.Key)
			s.Mu.Unlock()
			if err != nil {
				commandError(w, 503, "unknown", "persistence_unavailable")
				return
			}
		}
		for k, v := range result.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(result.Code)
		_, _ = w.Write(result.Body.Bytes())
	})
}

// WithContext carries an already-admitted command to its next executor hop.
// Receivers still validate and persist their own watermark before dispatch.
func WithContext(ctx context.Context, envelope Envelope) context.Context {
	return context.WithValue(ctx, contextKey{}, envelope)
}

// CheckCurrentLocked revalidates admitted authority after waiting for the host
// mutation lock. The caller must hold Journal.Mu through its effect (INV-04).
func (s *Journal) CheckCurrentLocked(ctx context.Context) error {
	envelope, ok := FromContext(ctx)
	if !ok {
		return &api.BoundaryError{Class: "denied", Code: "missing_identity", Message: "durable command identity is required"}
	}
	if err := s.validateAuthority(envelope, false); err != nil {
		return err
	}
	var token uint64
	if err := s.Conn.QueryRowContext(ctx, "SELECT token FROM command_fences WHERE subject=?1", envelope.Subject).Scan(&token); err != nil {
		return &api.BoundaryError{Class: "unknown", Code: "persistence_unavailable", Message: "command watermark is unavailable"}
	}
	if envelope.Token < token {
		return &api.BoundaryError{Class: "state", Code: "stale_fence", Message: "command ownership changed before dispatch"}
	}
	if envelope.Token != token {
		return &api.BoundaryError{Class: "unknown", Code: "identity_mismatch", Message: "command watermark does not match admission"}
	}
	return nil
}
