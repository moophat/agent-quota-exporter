package events

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Checkpoint reads adapter-owned durable progress, independently of metric state.
func (s *Store) Checkpoint(key string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.checkpointPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}
func (s *Store) checkpointPath(key string) string {
	h := sha256.Sum256([]byte(key))
	return filepath.Join(s.root, "cursor-"+hex.EncodeToString(h[:])+".json")
}

// SaveCheckpoint uses temp/fsync/rename/directory-fsync. Call outside Append's callback.
func (s *Store) SaveCheckpoint(key string, raw json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.checkpointPath(key)
	f, err := os.CreateTemp(s.root, ".cursor-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = s.sync(f)
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(s.root, s.sync)
}
