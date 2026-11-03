package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// InstState is the persisted part of one source instance.
type InstState struct {
	LastAttempt  int64           `json:"last_attempt,omitempty"` // unix seconds
	LastSuccess  int64           `json:"last_success,omitempty"`
	BackoffUntil int64           `json:"backoff_until,omitempty"`
	Plugin       json.RawMessage `json:"plugin,omitempty"`
}

type stateFile struct {
	Epoch     int64                 `json:"epoch"`
	Instances map[string]*InstState `json:"instances"`
}

// State is the exporter's only persistent store: state_dir/state.json, written atomically by one writer.
type State struct {
	mu   sync.Mutex
	dir  string
	f    stateFile
	lock *os.File
	// Quarantined is true when a corrupt state file was moved aside at start; callers then apply a
	// conservative backoff to network sources so lost gates cannot hammer a vendor.
	Quarantined bool
}

// OpenState creates/locks state_dir and loads state.json. A second process on the same dir fails.
func OpenState(dir string, now time.Time) (*State, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state_dir: cannot create (%s)", errClass(err))
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state_dir: cannot chmod (%s)", errClass(err))
	}
	lf, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("state_dir: cannot open lock (%s)", errClass(err))
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lf.Close()
		return nil, errors.New("state_dir is locked by another agent-quota-exporter process")
	}
	s := &State{dir: dir, lock: lf, f: stateFile{Epoch: now.Unix(), Instances: map[string]*InstState{}}}
	p := filepath.Join(dir, "state.json")
	b, err := os.ReadFile(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		s.Close()
		return nil, fmt.Errorf("state.json: cannot read (%s)", errClass(err))
	default:
		var f stateFile
		if json.Unmarshal(b, &f) != nil || f.Epoch == 0 {
			_ = os.Rename(p, fmt.Sprintf("%s.corrupt-%d", p, now.Unix()))
			s.Quarantined = true
		} else {
			if f.Instances == nil {
				f.Instances = map[string]*InstState{}
			}
			s.f = f
		}
	}
	return s, s.Save()
}

// Close releases the lock.
func (s *State) Close() {
	if s.lock != nil {
		s.lock.Close()
	}
}

// Epoch is the unix time the state was created (agent_quota_exporter_state_epoch).
func (s *State) Epoch() int64 { return s.f.Epoch }

// Get returns a copy of an instance's state (zero value if absent).
func (s *State) Get(key string) InstState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.f.Instances[key]; ok {
		return *v
	}
	return InstState{}
}

// Put replaces an instance's state in memory.
func (s *State) Put(key string, v InstState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := v
	s.f.Instances[key] = &cp
}

// Save writes state.json atomically (tmp + fsync + rename) with mode 0600.
func (s *State) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(s.f)
	if err != nil {
		return err
	}
	p := filepath.Join(s.dir, "state.json")
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("state.json: cannot write (%s)", errClass(err))
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("state.json: cannot write (%s)", errClass(err))
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("state.json: cannot chmod (%s)", errClass(err))
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("state.json: cannot rename (%s)", errClass(err))
	}
	return nil
}

// errClass reduces an OS error to a path-free class for logs.
func errClass(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return ReasonNotFound
	case errors.Is(err, os.ErrPermission):
		return ReasonPermission
	default:
		return ReasonOther
	}
}

// ErrReason maps a file-system error to a contract reason without exposing its text.
func ErrReason(err error) string { return errClass(err) }
