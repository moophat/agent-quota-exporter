// Package events persists private, allowlisted event envelopes as durable JSONL.
package events

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"syscall"
	"time"
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

var (
	ErrBudget    = errors.New("event store budget exceeded")
	ErrFreeSpace = errors.New("event store free-space reserve would be consumed")
)

// Envelope is the B01-required subset used to partition and validate an event.
// The complete schema remains owned by each source adapter.
type Envelope struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	EventID       string `json:"event_id"`
	Source        string `json:"source"`
	ParserVersion string `json:"parser_version"`
	Host          string `json:"host"`
	Profile       string `json:"profile"`
	Harness       string `json:"harness"`
}

var kinds = map[string]bool{
	"usage": true, "identity": true, "quota_observation": true, "quota_attempt": true,
	"collect": true, "mirai_summary": true, "mirai_request": true,
}

// Batch contains envelopes represented as complete JSON objects. Cursor is invoked only after every written shard
// has reached stable storage, allowing the caller to durably advance its source cursor in the required order.
type Batch struct {
	Envelopes []json.RawMessage
	Cursor    func() error
}

// Options controls a local event store. Budget and Reserve must both be positive.
type Options struct {
	Root    string
	Budget  int64
	Reserve int64
	Now     func() time.Time
	Sync    func(*os.File) error
	Free    func(string) (int64, error)
	Write   func(*os.File, []byte) (int, error)
}

// Store is a single-process durable event sink. Its root is private to the exporter.
type Store struct {
	root     string
	budget   int64
	reserve  int64
	now      func() time.Time
	sync     func(*os.File) error
	free     func(string) (int64, error)
	mu       sync.Mutex
	seen     map[string][32]byte
	used     int64
	pending  map[string]bool
	write    func(*os.File, []byte) (int, error)
	rollback map[string]int64
	outcomes map[Writer]bool
}

// Open creates a private store root and heals an incomplete final JSONL row before accepting new data.
func Open(opts Options) (*Store, error) {
	if opts.Root == "" || opts.Budget <= 0 || opts.Reserve <= 0 {
		return nil, errors.New("event store requires root, positive budget, and positive reserve")
	}
	if err := os.MkdirAll(opts.Root, 0o750); err != nil {
		return nil, fmt.Errorf("event store: create root: %w", err)
	}
	if err := os.Chmod(opts.Root, 0o750); err != nil {
		return nil, fmt.Errorf("event store: chmod root: %w", err)
	}
	s := &Store{root: opts.Root, budget: opts.Budget, reserve: opts.Reserve, now: opts.Now, sync: opts.Sync, free: opts.Free}
	if s.now == nil {
		s.now = time.Now
	}
	if s.sync == nil {
		s.sync = (*os.File).Sync
	}
	if s.free == nil {
		s.free = freeBytes
	}
	if err := s.heal(); err != nil {
		return nil, err
	}
	s.write = opts.Write
	if s.write == nil {
		s.write = (*os.File).Write
	}
	s.pending = map[string]bool{}
	s.rollback = map[string]int64{}
	s.outcomes = map[Writer]bool{}
	var err error
	s.seen, err = s.ids()
	if err != nil {
		return nil, err
	}
	s.used, err = treeSize(s.root)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Append durably writes a batch, then invokes its cursor checkpoint. A failed write or fsync never calls Cursor.
func (s *Store) Append(batch Batch) (result error) {
	if len(batch.Envelopes) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		for _, raw := range batch.Envelopes {
			e, err := parse(raw)
			if err == nil {
				s.outcomes[Writer{e.Host, e.Harness, e.Profile, e.Source}] = result == nil
			}
		}
	}()

	shards := map[string][][]byte{}
	if err := s.reconcile(); err != nil {
		return err
	}
	seen := map[string][32]byte{}
	for _, raw := range batch.Envelopes {
		e, err := parse(raw)
		if err != nil {
			return err
		}
		line, err := json.Marshal(json.RawMessage(raw))
		if err != nil {
			return fmt.Errorf("event store: encode envelope: %w", err)
		}
		line = append(line, '\n')
		key := e.EventID
		digest := sha256.Sum256(line[:len(line)-1])
		prior, ok := seen[key]
		if !ok {
			prior, ok = s.seen[key]
		}
		if ok {
			if prior != digest {
				return fmt.Errorf("event store: event ID %q has conflicting payload", key)
			}
			continue
		}
		seen[key] = digest
		month := s.now().UTC().Format("2006-01")
		path := filepath.Join(s.root, "events", "month="+month, e.Source+"-"+e.Profile+".jsonl")
		shards[path] = append(shards[path], line)
	}
	var bytes int64
	for _, lines := range shards {
		for _, line := range lines {
			bytes += int64(len(line))
		}
	}
	used := s.used
	if used+bytes > s.budget {
		return fmt.Errorf("%w: used=%d batch=%d budget=%d", ErrBudget, used, bytes, s.budget)
	}
	if bytes == 0 {
		return checkpoint(batch)
	}
	free, err := s.free(s.root)
	if err != nil {
		return fmt.Errorf("event store: measure free space: %w", err)
	}
	// Allow block rounding plus conservative inode/directory allocation per shard.
	allocation := bytes + int64(len(shards))*65536
	if free-allocation <= s.reserve {
		return fmt.Errorf("%w: available=%d batch=%d reserve=%d", ErrFreeSpace, free, bytes, s.reserve)
	}

	paths := make([]string, 0, len(shards))
	for path := range shards {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := s.mkdir(filepath.Dir(path)); err != nil {
			return fmt.Errorf("event store: create shard directory: %w", err)
		}
		if err := os.Chmod(filepath.Dir(path), 0o750); err != nil {
			return fmt.Errorf("event store: chmod shard directory: %w", err)
		}
		if err := syncDir(filepath.Dir(path), s.sync); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		if err != nil {
			return fmt.Errorf("event store: open shard: %w", err)
		}
		s.pending[path] = true
		for _, line := range shards[path] {
			info, statErr := f.Stat()
			if statErr != nil {
				f.Close()
				return statErr
			}
			n, writeErr := s.write(f, line)
			if writeErr != nil || n != len(line) {
				s.rollback[path] = info.Size()
				trimErr := f.Truncate(info.Size())
				if trimErr == nil {
					delete(s.rollback, path)
				}
				err = errors.Join(writeErr, io.ErrShortWrite, trimErr)
				f.Close()
				return fmt.Errorf("event store: append shard: %w", err)
			}
			s.used += int64(n)
			var e Envelope
			_ = json.Unmarshal(line, &e)
			s.seen[e.EventID] = sha256.Sum256(line[:len(line)-1])
		}
		if err = s.sync(f); err == nil {
			err = f.Close()
		} else {
			f.Close()
		}
		if err != nil {
			return fmt.Errorf("event store: sync shard: %w", err)
		}
		if err := os.Chmod(path, 0o640); err != nil {
			return fmt.Errorf("event store: chmod shard: %w", err)
		}
		if err := syncDir(filepath.Dir(path), s.sync); err != nil {
			return err
		}
		delete(s.pending, path)
	}
	return checkpoint(batch)
}

func checkpoint(batch Batch) error {
	if batch.Cursor != nil {
		if err := batch.Cursor(); err != nil {
			return fmt.Errorf("event store: cursor checkpoint: %w", err)
		}
	}
	return nil
}

// Replay calls fn for every intact envelope in lexical shard order.
func (s *Store) Replay(fn func(json.RawMessage) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var paths []string
	err := filepath.WalkDir(filepath.Join(s.root, "events"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return err
		}
		paths = append(paths, path)
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("event store: list shards: %w", err)
	}
	sort.Strings(paths)
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("event store: open replay shard: %w", err)
		}
		reader := bufio.NewReader(f)
		for {
			line, readErr := reader.ReadBytes('\n')
			if readErr == io.EOF && len(line) == 0 {
				break
			}
			if readErr != nil {
				f.Close()
				return readErr
			}
			if _, err := parse(line); err != nil {
				f.Close()
				return fmt.Errorf("event store: invalid stored envelope: %w", err)
			}
			if err := fn(line); err != nil {
				f.Close()
				return err
			}
		}
		f.Close()
	}
	return nil
}

func (s *Store) heal() error {
	root := filepath.Join(s.root, "events")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0o750)
		}
		if filepath.Ext(path) != ".jsonl" {
			return nil
		}
		if err := os.Chmod(path, 0o640); err != nil {
			return err
		}
		return healTail(path)
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func healTail(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("event store: open shard for recovery: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	end := info.Size()
	buf := make([]byte, 4096)
	cut := int64(0)
	for end > 0 {
		start := end - int64(len(buf))
		if start < 0 {
			start = 0
		}
		n, err := f.ReadAt(buf[:end-start], start)
		if err != nil {
			return err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			cut = start + int64(i) + 1
			break
		}
		end = start
	}
	if cut == info.Size() {
		return nil
	}
	if err := f.Truncate(cut); err != nil {
		return fmt.Errorf("event store: trim torn tail: %w", err)
	}
	return f.Sync()
}

func parse(raw []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return e, fmt.Errorf("invalid JSON envelope: %w", err)
	}
	if e.SchemaVersion != 1 || !kinds[e.Kind] || e.EventID == "" || !valid(e.Source) || e.ParserVersion == "" || e.Host == "" || !valid(e.Profile) || e.Harness == "" {
		return e, errors.New("invalid required envelope fields")
	}
	if err := validateEnvelope(raw, e.Kind); err != nil {
		return e, err
	}
	return e, nil
}

func (s *Store) ids() (map[string][32]byte, error) {
	ids := map[string][32]byte{}
	root := filepath.Join(s.root, "events")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		reader := bufio.NewReader(f)
		for {
			raw, err := reader.ReadBytes('\n')
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
			digest := sha256.Sum256(compact)
			if prior, ok := ids[e.EventID]; ok && prior != digest {
				return fmt.Errorf("event store: event ID %q has conflicting payload", e.EventID)
			}
			ids[e.EventID] = digest
		}
		// Reopening must establish durability even for complete rows left by failed syncs.
		if err := s.sync(f); err != nil {
			return err
		}
		return s.mkdir(filepath.Dir(path))
	})
	if errors.Is(err, os.ErrNotExist) {
		return ids, nil
	}
	return ids, err
}

func syncDir(path string, sync func(*os.File) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("event store: open shard directory: %w", err)
	}
	defer f.Close()
	if err := sync(f); err != nil {
		return fmt.Errorf("event store: sync shard directory: %w", err)
	}
	return nil
}

func valid(s string) bool { return identifier.MatchString(s) }

func treeSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func freeBytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}
