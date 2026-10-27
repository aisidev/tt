package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/aisidev/tt/model"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"github.com/aisidev/tt/api"
	"github.com/google/uuid"
)

const EpochSequenceBits = 40
const MaxEpoch = (1 << 23) - 1

// AuthorityState lives outside the SQLite restore scope. Root operators advance
// it before recovery; a restored database cannot lower this receiver's epoch.
// Epochs alone are not physical host fencing or permission to resume work.
type AuthorityState struct {
	Version int    `json:"version"`
	ID      string `json:"authority"`
	Epoch   uint64 `json:"epoch"`
	Phase   string `json:"phase"`
	Key     string `json:"key"`
}
type Authority struct {
	path    string
	mu      sync.Mutex
	highest uint64
	id      string
	key     string
}

func authorityError(class, code string) error {
	return &api.BoundaryError{Class: class, Code: code, Message: "receiver authority is unavailable or does not authorize this command"}
}
func OpenAuthority(path string) (*Authority, error) {
	if !filepath.IsAbs(path) {
		return nil, authorityError("terminal", "invalid_authority")
	}
	authority := &Authority{path: path}
	if _, err := authority.State(); err != nil {
		return nil, err
	}
	return authority, nil
}
func (a *Authority) State() (AuthorityState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var state AuthorityState
	file, err := os.OpenFile(a.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return state, authorityError("unknown", "authority_unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return state, authorityError("unknown", "authority_unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return state, authorityError("terminal", "malformed_authority")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return state, authorityError("unknown", "authority_unavailable")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > 4096 || decoder.Decode(&state) != nil || decoder.Decode(new(any)) != io.EOF || state.Version != 1 || state.Epoch < 1 || state.Epoch > MaxEpoch {
		return state, authorityError("terminal", "malformed_authority")
	}
	identity, identityErr := uuid.Parse(state.ID)
	key, keyErr := uuid.Parse(state.Key)
	if identityErr != nil || keyErr != nil || identity.String() != state.ID || key.String() != state.Key {
		return state, authorityError("terminal", "malformed_authority")
	}
	switch state.Phase {
	case "fenced", "inventory", "reads", "mutations", "gc":
	default:
		return state, authorityError("terminal", "malformed_authority")
	}
	if state.Epoch < a.highest || (a.id != "" && a.id != state.ID) || state.Epoch == a.highest && a.key != "" && a.key != state.Key {
		return state, authorityError("state", "authority_rollback")
	}
	a.highest = state.Epoch
	a.id = state.ID
	a.key = state.Key
	return state, nil
}
func (s *Journal) SetAuthority(path string) error {
	if path == "" {
		return nil
	}
	authority, err := OpenAuthority(path)
	if err != nil {
		return err
	}
	s.Authority = authority
	return nil
}
func (s *Journal) MutationsAllowed() bool {
	if s.Authority == nil {
		return true
	}
	state, err := s.Authority.State()
	return err == nil && (state.Phase == "mutations" || state.Phase == "gc") && s.Authority.Active("mutations")
}
func (s *Journal) validateAuthority(cmd Envelope, recovery bool) error {
	if s.Authority == nil {
		return nil
	}
	state, err := s.Authority.State()
	if err != nil {
		return err
	}
	if cmd.Authority != state.ID || cmd.Epoch != state.Epoch || cmd.Token>>EpochSequenceBits != state.Epoch {
		return authorityError("state", "stale_epoch")
	}
	if recovery {
		return nil
	}
	return s.Authority.RequireActive("mutations")
}

func (s *Journal) ControlReport() (model.ControlReport, error) {
	report := model.ControlReport{ProtocolVersion: 1, Phase: "legacy"}
	if s.Authority == nil {
		return report, nil
	}
	state, err := s.Authority.State()
	if err != nil {
		return report, err
	}
	report.Enabled = true
	report.ActivationVersion = 2
	report.Authority = state.ID
	report.Epoch = state.Epoch
	report.Phase = state.Phase
	report.Fence, err = s.Authority.FenceReceipt()
	if err != nil {
		return report, err
	}
	for _, phase := range []string{"reads", "mutations", "gc"} {
		activation, readErr := s.Authority.Activation(phase)
		if readErr != nil {
			return report, readErr
		}
		if activation != nil {
			report.Activations = append(report.Activations, *activation)
		}
	}
	return report, err
}
func (s *Journal) ControlHTTP(w http.ResponseWriter, r *http.Request) {
	report, err := s.ControlReport()
	if err != nil {
		var typed *api.BoundaryError
		if errors.As(err, &typed) {
			api.WriteJSON(w, typed.Status(), api.Resp[model.ControlReport]{Failure: typed})
		} else {
			commandError(w, 503, "unknown", "authority_unavailable")
		}
		return
	}
	api.WriteJSON(w, 200, api.Success(report))
}

func (s *Journal) ControlSubject() (string, error) {
	if s.Authority == nil {
		return "", authorityError("terminal", "unimplemented")
	}
	state, err := s.Authority.State()
	if err != nil {
		return "", err
	}
	return "control-" + state.ID + "-g" + strconv.FormatUint(state.Epoch, 10), nil
}
func (s *Journal) IdleExcept(subject string) bool {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	for id := range s.busy {
		if id != subject {
			return false
		}
	}
	return true
}
func (a *Authority) writeDocument(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".control-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	n, writeErr := file.Write(raw)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || n != len(raw) || syncErr != nil || closeErr != nil {
		return authorityError("unknown", "authority_unavailable")
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
func (a *Authority) CommitFence(receipt model.FencingReceipt) error {
	lock, err := os.OpenFile(a.path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return authorityError("unknown", "authority_unavailable")
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return authorityError("state", "authority_busy")
	}
	state, err := a.State()
	if err != nil {
		return err
	}
	if state.ID != receipt.Authority || state.Epoch != receipt.Epoch || (state.Phase != "fenced" && state.Phase != "inventory") {
		return authorityError("state", "stale_epoch")
	}
	if err = a.writeDocument(a.path+".fence", receipt); err != nil {
		return authorityError("unknown", "authority_unavailable")
	}
	state.Phase = "inventory"
	if err = a.writeDocument(a.path, state); err != nil {
		return authorityError("unknown", "authority_unavailable")
	}
	return nil
}
func (a *Authority) FenceReceipt() (*model.FencingReceipt, error) {
	state, err := a.State()
	if err != nil {
		return nil, err
	}
	var receipt model.FencingReceipt
	found, err := readControlDocument(a.path+".fence", &receipt)
	if err != nil || !found {
		return nil, err
	}
	if receipt.Version != 1 {
		return nil, authorityError("terminal", "malformed_authority")
	}
	if receipt.Authority != state.ID || receipt.Epoch != state.Epoch {
		return nil, nil
	}
	return &receipt, nil
}
