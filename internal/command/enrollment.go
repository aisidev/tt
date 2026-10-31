package command

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

const MaxEnrollmentGrants = 256
const maxEnrollmentCatalog = 256 << 10

// EnrollmentStore is independent of SQLite restore. Its generated directory
// identity, separately anchored count and paired claim records prevent missing
// metadata from manufacturing fresh enrollment permission (INV-03/04/05/14).
type EnrollmentStore struct{ Directory string }
type enrollmentOwner struct {
	CreatedBy string `json:"created_by"`
	Version   int    `json:"version"`
	ID        string `json:"id"`
	Count     int    `json:"count"`
}
type enrollmentGrant struct {
	ReceiptSHA256 string                  `json:"receipt_sha256,omitempty"`
	Authority     string                  `json:"authority"`
	Epoch         uint64                  `json:"epoch"`
	Request       model.EnrollmentRequest `json:"request"`
	TokenSHA256   string                  `json:"token_sha256"`
	ClaimedBy     string                  `json:"claimed_by,omitempty"`
}
type enrollmentCatalog struct {
	Version int                        `json:"version"`
	ID      string                     `json:"id"`
	Grants  map[string]enrollmentGrant `json:"grants"`
}
type enrollmentClaim struct {
	Receipt *model.EnrollmentReceipt `json:"receipt,omitempty"`
	Version int                      `json:"version"`
	Key     string                   `json:"key"`
	Request model.EnrollmentRequest  `json:"request"`
}

func enrollmentError(class, code string) error { return authorityError(class, code) }
func enrollmentSHA(b []byte) string            { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func enrollmentRead(path string, limit int64, out any) error {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return enrollmentError("unknown", "enrollment_evidence_unavailable")
	}
	defer f.Close()
	st, e := f.Stat()
	var identity syscall.Stat_t
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() < 1 || st.Size() > limit || syscall.Fstat(int(f.Fd()), &identity) != nil || identity.Uid != uint32(os.Geteuid()) || identity.Nlink != 1 {
		return enrollmentError("unknown", "enrollment_evidence_unavailable")
	}
	dec := json.NewDecoder(io.LimitReader(f, limit+1))
	dec.DisallowUnknownFields()
	if dec.Decode(out) != nil || dec.Decode(new(any)) != io.EOF {
		return enrollmentError("unknown", "enrollment_evidence_unavailable")
	}
	return nil
}
func enrollmentExclusive(path string, b []byte) error {
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return enrollmentError("unknown", "enrollment_evidence_unavailable")
	}
	n, w := f.Write(b)
	s := f.Sync()
	c := f.Close()
	if w != nil || s != nil || c != nil || n != len(b) {
		return enrollmentError("unknown", "enrollment_evidence_unavailable")
	}
	return nil
}
func enrollmentSync(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func enrollmentJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func enrollmentReplace(directory, name string, value any) error {
	data := enrollmentJSON(value)
	if len(data) > maxEnrollmentCatalog {
		return enrollmentError("rate_limited", "quota_exceeded")
	}
	temp := filepath.Join(directory, "."+name+".new")
	if e := enrollmentExclusive(temp, data); e != nil {
		return e
	}
	if e := os.Rename(temp, filepath.Join(directory, name)); e != nil {
		return enrollmentError("unknown", "enrollment_evidence_unavailable")
	}
	return enrollmentSync(directory)
}

// CreateEnrollmentStore publishes a fresh generated directory below an existing
// private parent. It cannot recreate a deleted configured identity by name.
func CreateEnrollmentStore(parent string, key string) (EnrollmentStore, error) {
	var empty EnrollmentStore
	if !enrollmentUUID(key) || key == uuid.Nil.String() {
		return empty, enrollmentError("terminal", "invalid_identity")
	}
	if !filepath.IsAbs(parent) || filepath.Clean(parent) != parent {
		return empty, enrollmentError("terminal", "invalid_enrollment_store")
	}
	st, e := os.Lstat(parent)
	if e != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return empty, enrollmentError("denied", "invalid_enrollment_store")
	}
	lock, e := os.OpenFile(filepath.Join(parent, ".enrollment-init.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if e != nil {
		return empty, e
	}
	defer lock.Close()
	var ownerStat syscall.Stat_t
	if syscall.Fstat(int(lock.Fd()), &ownerStat) != nil || ownerStat.Mode&syscall.S_IFMT != syscall.S_IFREG || ownerStat.Mode&0077 != 0 || ownerStat.Nlink != 1 || ownerStat.Uid != uint32(os.Geteuid()) {
		return empty, enrollmentError("denied", "invalid_enrollment_store")
	}
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return empty, enrollmentError("state", "enrollment_busy")
	}
	parentFile, e := os.OpenFile(parent, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return empty, e
	}
	entries, e := parentFile.ReadDir(MaxEnrollmentGrants + 2)
	_ = parentFile.Close()
	if e != nil && e != io.EOF {
		return empty, e
	}
	if len(entries) > MaxEnrollmentGrants+1 {
		return empty, enrollmentError("rate_limited", "quota_exceeded")
	}
	for _, entry := range entries {
		if entry.Name() == ".enrollment-init.lock" {
			continue
		}
		if !enrollmentUUID(entry.Name()) {
			return empty, enrollmentError("unknown", "enrollment_evidence_unavailable")
		}
		candidate := EnrollmentStore{Directory: filepath.Join(parent, entry.Name())}
		var owner enrollmentOwner
		if err := enrollmentRead(filepath.Join(candidate.Directory, "owner.json"), 4096, &owner); err != nil {
			return empty, err
		}
		if owner.CreatedBy == key {
			if e := candidate.Validate(); e != nil {
				return empty, e
			}
			return candidate, nil
		}
	}
	if len(entries)-1 >= MaxEnrollmentGrants {
		return empty, enrollmentError("rate_limited", "quota_exceeded")
	}
	id := uuid.NewString()
	path := filepath.Join(parent, id)
	if e = os.Mkdir(path, 0700); e != nil {
		return empty, e
	}
	if e = enrollmentExclusive(filepath.Join(path, "owner.json"), enrollmentJSON(enrollmentOwner{Version: 1, ID: id, CreatedBy: key})); e != nil {
		return empty, e
	}
	if e = enrollmentExclusive(filepath.Join(path, "catalog.json"), enrollmentJSON(enrollmentCatalog{Version: 1, ID: id, Grants: map[string]enrollmentGrant{}})); e != nil {
		return empty, e
	}
	if e = enrollmentSync(path); e != nil {
		return empty, e
	}
	if e = enrollmentSync(parent); e != nil {
		return empty, e
	}
	return EnrollmentStore{Directory: path}, nil
}
func (s EnrollmentStore) lock() (*os.File, error) {
	if !filepath.IsAbs(s.Directory) || filepath.Clean(s.Directory) != s.Directory || !enrollmentUUID(filepath.Base(s.Directory)) {
		return nil, enrollmentError("terminal", "invalid_enrollment_store")
	}
	var st syscall.Stat_t
	if syscall.Lstat(s.Directory, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFDIR || st.Mode&0077 != 0 || st.Uid != uint32(os.Geteuid()) {
		return nil, enrollmentError("unknown", "enrollment_evidence_unavailable")
	}
	f, e := os.OpenFile(filepath.Join(s.Directory, "store.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if e != nil {
		return nil, e
	}
	if syscall.Fstat(int(f.Fd()), &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Mode&0077 != 0 || st.Nlink != 1 || st.Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, enrollmentError("unknown", "enrollment_evidence_unavailable")
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return nil, enrollmentError("state", "enrollment_busy")
	}
	return f, nil
}
func validEnrollmentRequest(r model.EnrollmentRequest) bool {
	u, e := url.Parse(r.Endpoint)
	return e == nil && u.Scheme == "http" && u.Host != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && model.ValidateName(r.HostID, "host_id") == nil && digestValid(r.ScopeDigest) && digestValid(r.PlanDigest)
}
func (s EnrollmentStore) load() (enrollmentOwner, enrollmentCatalog, error) {
	var o enrollmentOwner
	var c enrollmentCatalog
	if e := enrollmentRead(filepath.Join(s.Directory, "owner.json"), 4096, &o); e != nil {
		return o, c, e
	}
	if e := enrollmentRead(filepath.Join(s.Directory, "catalog.json"), maxEnrollmentCatalog, &c); e != nil {
		return o, c, e
	}
	if o.Version != 1 || !enrollmentUUID(o.CreatedBy) || o.ID != filepath.Base(s.Directory) || c.Version != 1 || c.ID != o.ID || c.Grants == nil || o.Count != len(c.Grants) || o.Count > MaxEnrollmentGrants {
		return o, c, enrollmentError("unknown", "enrollment_evidence_unavailable")
	}
	for id, g := range c.Grants {
		if !enrollmentUUID(id) || !enrollmentUUID(g.Authority) || g.Epoch == 0 || g.Epoch > MaxEpoch || !validEnrollmentRequest(g.Request) || !digestValid(g.TokenSHA256) || g.ClaimedBy != "" && !enrollmentUUID(g.ClaimedBy) {
			return o, c, enrollmentError("unknown", "enrollment_evidence_unavailable")
		}
		claimPath := filepath.Join(s.Directory, id+".claim")
		if g.ClaimedBy != "" {
			var claim enrollmentClaim
			if e := enrollmentRead(claimPath, 4096, &claim); e != nil {
				return o, c, e
			}
			if claim.Version != 1 || claim.Key != g.ClaimedBy || claim.Request != g.Request || claim.Receipt != nil && (claim.Receipt.Version != 1 || !enrollmentUUID(claim.Receipt.ID) || claim.Receipt.Key != g.ClaimedBy || claim.Receipt.Authority != g.Authority || claim.Receipt.Epoch != g.Epoch || claim.Receipt.Request != g.Request) {
				return o, c, enrollmentError("unknown", "enrollment_evidence_unavailable")
			}
			if (g.ReceiptSHA256 != "") != (claim.Receipt != nil) || g.ReceiptSHA256 != "" && (!digestValid(g.ReceiptSHA256) || enrollmentSHA(enrollmentJSON(claim.Receipt)) != g.ReceiptSHA256) {
				return o, c, enrollmentError("unknown", "enrollment_evidence_unavailable")
			}
		} else if _, e := os.Lstat(claimPath); !os.IsNotExist(e) {
			return o, c, enrollmentError("unknown", "enrollment_evidence_unavailable")
		}
	}
	return o, c, nil
}
func (s EnrollmentStore) Validate() error {
	f, e := s.lock()
	if e != nil {
		return e
	}
	defer f.Close()
	_, _, e = s.load()
	return e
}

// Issue binds a random token to one authority epoch and exact reviewed receiver.
// Only its private file contains the token value; records retain its digest.
// Exact issuance replay returns the original value, never new authority.
func (s EnrollmentStore) Issue(authority string, epoch uint64, key string, r model.EnrollmentRequest) (string, error) {
	if !enrollmentUUID(authority) || epoch == 0 || epoch > MaxEpoch || !enrollmentUUID(key) || !validEnrollmentRequest(r) {
		return "", enrollmentError("terminal", "invalid_enrollment_request")
	}
	f, e := s.lock()
	if e != nil {
		return "", e
	}
	defer f.Close()
	o, c, e := s.load()
	if e != nil {
		return "", e
	}
	tokenFile := filepath.Join(s.Directory, key+".token")
	if old, ok := c.Grants[key]; ok {
		if old.Authority != authority || old.Epoch != epoch || old.Request != r {
			return "", enrollmentError("terminal", "fingerprint_conflict")
		}
		var token struct {
			Value string `json:"value"`
		}
		if e = enrollmentRead(tokenFile, 4096, &token); e != nil {
			return "", e
		}
		if enrollmentSHA([]byte(token.Value)) != old.TokenSHA256 {
			return "", enrollmentError("unknown", "enrollment_evidence_unavailable")
		}
		return token.Value, nil
	}
	if o.Count >= MaxEnrollmentGrants {
		return "", enrollmentError("rate_limited", "quota_exceeded")
	}
	secret := make([]byte, 32)
	if _, e = rand.Read(secret); e != nil {
		return "", e
	}
	value := key + "." + hex.EncodeToString(secret)
	clear(secret)
	c.Grants[key] = enrollmentGrant{Authority: authority, Epoch: epoch, Request: r, TokenSHA256: enrollmentSHA([]byte(value))}
	if len(enrollmentJSON(c)) > maxEnrollmentCatalog {
		return "", enrollmentError("rate_limited", "quota_exceeded")
	}
	if e = enrollmentExclusive(tokenFile, enrollmentJSON(struct {
		Value string `json:"value"`
	}{value})); e != nil {
		return "", e
	}
	o.Count++
	if e = enrollmentReplace(s.Directory, "owner.json", o); e != nil {
		return "", e
	}
	if e = enrollmentReplace(s.Directory, "catalog.json", c); e != nil {
		return "", e
	}
	return value, nil
}
func (s EnrollmentStore) verify(c enrollmentCatalog, token string, env Envelope, r model.EnrollmentRequest) (string, enrollmentGrant, error) {
	var empty enrollmentGrant
	parts := strings.Split(token, ".")
	if len(parts) != 2 || !enrollmentUUID(parts[0]) || len(parts[1]) != 64 {
		return "", empty, enrollmentError("denied", "enrollment_token_invalid")
	}
	grant, ok := c.Grants[parts[0]]
	actual := enrollmentSHA([]byte(token))
	if !ok || subtle.ConstantTimeCompare([]byte(actual), []byte(grant.TokenSHA256)) != 1 || grant.Authority != env.Authority || grant.Epoch != env.Epoch || grant.Request != r {
		return "", empty, enrollmentError("denied", "enrollment_token_invalid")
	}
	if grant.ClaimedBy != "" && grant.ClaimedBy != env.Key {
		return "", empty, enrollmentError("denied", "enrollment_token_used")
	}
	return parts[0], grant, nil
}
func (s EnrollmentStore) Check(token string, env Envelope, r model.EnrollmentRequest) error {
	f, e := s.lock()
	if e != nil {
		return e
	}
	defer f.Close()
	_, c, e := s.load()
	if e != nil {
		return e
	}
	_, _, e = s.verify(c, token, env, r)
	return e
}

// Claim commits before membership mutation. Reconciliation may reuse only the
// exact original command identity; another key never reuses this grant.
func (s EnrollmentStore) Claim(token string, env Envelope, r model.EnrollmentRequest) error {
	if !enrollmentUUID(env.Key) {
		return enrollmentError("terminal", "invalid_identity")
	}
	f, e := s.lock()
	if e != nil {
		return e
	}
	defer f.Close()
	_, c, e := s.load()
	if e != nil {
		return e
	}
	id, g, e := s.verify(c, token, env, r)
	if e != nil {
		return e
	}
	if g.ClaimedBy != "" {
		return nil
	}
	g.ClaimedBy = env.Key
	c.Grants[id] = g
	if e = enrollmentReplace(s.Directory, "catalog.json", c); e != nil {
		return e
	}
	if e = enrollmentExclusive(filepath.Join(s.Directory, id+".claim"), enrollmentJSON(enrollmentClaim{Version: 1, Key: env.Key, Request: r})); e != nil {
		return e
	}
	return enrollmentSync(s.Directory)
}

// Receipt replays the independently retained original business result even if
// the SQLite command-cache publication was lost. It does not probe membership.
func (s EnrollmentStore) Receipt(token string, env Envelope, r model.EnrollmentRequest) (*model.EnrollmentReceipt, error) {
	f, e := s.lock()
	if e != nil {
		return nil, e
	}
	defer f.Close()
	_, c, e := s.load()
	if e != nil {
		return nil, e
	}
	id, g, e := s.verify(c, token, env, r)
	if e != nil {
		return nil, e
	}
	if g.ClaimedBy == "" {
		return nil, nil
	}
	var claim enrollmentClaim
	if e = enrollmentRead(filepath.Join(s.Directory, id+".claim"), 4096, &claim); e != nil {
		return nil, e
	}
	return claim.Receipt, nil
}
func (s EnrollmentStore) Complete(token string, env Envelope, r model.EnrollmentRequest, receipt model.EnrollmentReceipt) error {
	f, e := s.lock()
	if e != nil {
		return e
	}
	defer f.Close()
	_, c, e := s.load()
	if e != nil {
		return e
	}
	id, g, e := s.verify(c, token, env, r)
	if e != nil {
		return e
	}
	if g.ClaimedBy != env.Key || receipt.Version != 1 || !enrollmentUUID(receipt.ID) || receipt.Key != env.Key || receipt.Authority != env.Authority || receipt.Epoch != env.Epoch || receipt.Request != r {
		return enrollmentError("terminal", "fingerprint_conflict")
	}
	var claim enrollmentClaim
	if e = enrollmentRead(filepath.Join(s.Directory, id+".claim"), 4096, &claim); e != nil {
		return e
	}
	if claim.Receipt != nil {
		if *claim.Receipt != receipt {
			return enrollmentError("terminal", "fingerprint_conflict")
		}
		return nil
	}
	g.ReceiptSHA256 = enrollmentSHA(enrollmentJSON(&receipt))
	c.Grants[id] = g
	if e = enrollmentReplace(s.Directory, "catalog.json", c); e != nil {
		return e
	}
	claim.Receipt = &receipt
	return enrollmentReplace(s.Directory, id+".claim", claim)
}

func enrollmentUUID(value string) bool { return canonicalUUID(value) && value != uuid.Nil.String() }
