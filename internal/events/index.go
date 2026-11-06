package events

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Close releases the exporter-owned index connection after source workers stop.
func (s *Store) Close() error {
	if s == nil || s.index == nil {
		return nil
	}
	return s.index.Close()
}

// The exporter-owned SQLite index is an accelerator, not the event authority.
// Checkpoints are committed only after shard fsync. Recovery reads just the
// unindexed suffix; missing index (migration/loss) rebuilds once from raw shards.
func (s *Store) openIndex() error {
	path := filepath.Join(s.root, "index.sqlite")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0640)
	if err != nil {
		return err
	}
	f.Close()
	if err = os.Chmod(path, 0640); err != nil {
		return err
	}
	s.index, err = sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	s.index.SetMaxOpenConns(1)
	_, err = s.index.Exec(`PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL;
	 CREATE TABLE IF NOT EXISTS ids (id TEXT PRIMARY KEY, digest BLOB NOT NULL) WITHOUT ROWID;
	 CREATE TABLE IF NOT EXISTS shards (path TEXT PRIMARY KEY, offset INTEGER NOT NULL) WITHOUT ROWID;`)
	if err != nil {
		return err
	}
	return s.recoverIndex()
}

func (s *Store) indexed(id string) ([32]byte, bool, error) {
	var result [32]byte
	var b []byte
	err := s.index.QueryRow("SELECT digest FROM ids WHERE id=?", id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	if len(b) != 32 {
		return result, false, errors.New("invalid event index digest")
	}
	copy(result[:], b)
	return result, true, nil
}

// Contains consults the durable index without replaying or retaining history.
// Reconciliation makes complete rows from a failed sync durable before reuse.
func (s *Store) Contains(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reconcile(); err != nil {
		return false, err
	}
	_, ok, err := s.indexed(id)
	return ok, err
}

func (s *Store) recoverIndex() error {
	return walkShards(s.root, s.indexShard)
}
func (s *Store) indexShard(path string) error {
	var offset int64
	err := s.index.QueryRow("SELECT offset FROM shards WHERE path=?", path).Scan(&offset)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if offset > info.Size() {
		return errors.New("indexed event shard shrank")
	}
	if offset == info.Size() {
		return nil
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	tx, err := s.index.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r := bufio.NewReader(f)
	for {
		raw, err := r.ReadBytes('\n')
		if err == io.EOF && len(raw) == 0 {
			break
		}
		if err != nil {
			return err
		}
		e, err := parse(raw)
		if err != nil {
			return err
		}
		compact, err := json.Marshal(json.RawMessage(raw))
		if err != nil {
			return err
		}
		h := sha256.Sum256(compact)
		var prior []byte
		err = tx.QueryRow("SELECT digest FROM ids WHERE id=?", e.EventID).Scan(&prior)
		if err == nil && string(prior) != string(h[:]) {
			return fmt.Errorf("event store: event ID %q has conflicting payload", e.EventID)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err = tx.Exec("INSERT OR IGNORE INTO ids VALUES (?,?)", e.EventID, h[:]); err != nil {
			return err
		}
		offset += int64(len(raw))
	}
	if err = s.sync(f); err != nil {
		return err
	}
	if err = s.mkdir(filepath.Dir(path)); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT OR REPLACE INTO shards VALUES (?,?)", path, offset); err != nil {
		return err
	}
	return tx.Commit()
}

func walkShards(root string, fn func(string) error) error {
	err := filepath.WalkDir(filepath.Join(root, "events"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		return fn(path)
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
