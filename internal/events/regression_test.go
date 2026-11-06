package events

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFailedFileSyncRetryRequiresDurability(t *testing.T) {
	opts := options(t.TempDir())
	fail := true
	fileSyncs := 0
	opts.Sync = func(f *os.File) error {
		info, _ := f.Stat()
		if !info.IsDir() {
			fileSyncs++
			if fail {
				return errors.New("sync failure")
			}
		}
		return f.Sync()
	}
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	advanced := false
	b := Batch{Envelopes: []json.RawMessage{event("a")}, Cursor: func() error { advanced = true; return nil }}
	if s.Append(b) == nil || advanced {
		t.Fatal("failed sync acknowledged")
	}
	if s.Append(b) == nil || advanced || fileSyncs < 2 {
		t.Fatal("retry bypassed failed sync")
	}
	fail = false
	if err = s.Append(b); err != nil || !advanced {
		t.Fatalf("retry: %v", err)
	}
}

func TestMonthParentSyncBeforeCursor(t *testing.T) {
	opts := options(t.TempDir())
	fail := true
	opts.Sync = func(f *os.File) error {
		if filepath.Base(f.Name()) == "events" && fail {
			return errors.New("parent sync")
		}
		return f.Sync()
	}
	s, _ := Open(opts)
	advanced := false
	b := Batch{Envelopes: []json.RawMessage{event("a")}, Cursor: func() error { advanced = true; return nil }}
	if s.Append(b) == nil || advanced {
		t.Fatal("unsynced month acknowledged")
	}
	fail = false
	if err := s.Append(b); err != nil || !advanced {
		t.Fatalf("retry %v", err)
	}
}

func TestPartialWriteRecoveryWithoutReopen(t *testing.T) {
	opts := options(t.TempDir())
	fail := true
	opts.Write = func(f *os.File, b []byte) (int, error) {
		if fail {
			n, _ := f.Write(b[:len(b)/2])
			return n, io.ErrShortWrite
		}
		return f.Write(b)
	}
	s, _ := Open(opts)
	b := Batch{Envelopes: []json.RawMessage{event("a")}}
	if s.Append(b) == nil {
		t.Fatal("partial write succeeded")
	}
	fail = false
	if err := s.Append(b); err != nil {
		t.Fatal(err)
	}
	count := 0
	if err := s.Replay(func(json.RawMessage) error { count++; return nil }); err != nil || count != 1 {
		t.Fatalf("count %d: %v", count, err)
	}
}

func TestClosedSchemasAndRequiredEnvelope(t *testing.T) {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(event("a"), &m)
	for k := range m {
		t.Run("missing-"+k, func(t *testing.T) {
			copy := map[string]json.RawMessage{}
			for key, v := range m {
				if key != k {
					copy[key] = v
				}
			}
			raw, _ := json.Marshal(copy)
			s, _ := Open(options(t.TempDir()))
			if s.Append(Batch{Envelopes: []json.RawMessage{raw}}) == nil {
				t.Fatal("accepted missing key")
			}
		})
	}
	for _, data := range []string{`{"token":"invented"}`, `{"model":{"content":"invented"}}`, `{"model":"Bearer invented"}`, `{"raw_tokens":{"prompt":"invented"}}`} {
		m["data"] = json.RawMessage(data)
		raw, _ := json.Marshal(m)
		s, _ := Open(options(t.TempDir()))
		if s.Append(Batch{Envelopes: []json.RawMessage{raw}}) == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestLargeRowReplayReopenAndLaterAppend(t *testing.T) {
	opts := options(t.TempDir())
	opts.Budget = 64 << 20
	s, _ := Open(opts)
	raw := strings.Replace(string(event("a")), `"data":{}`, `"data":{"model":"`+strings.Repeat("x", 17<<20)+`"}`, 1)
	if err := s.Append(Batch{Envelopes: []json.RawMessage{json.RawMessage(raw)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Replay(func(json.RawMessage) error { return nil }); err != nil {
		t.Fatal(err)
	}
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Append(Batch{Envelopes: []json.RawMessage{event("b")}}); err != nil {
		t.Fatal(err)
	}
}

func TestWriteFreeRetryAtFullStore(t *testing.T) {
	opts := options(t.TempDir())
	free := int64(1 << 30)
	opts.Free = func(string) (int64, error) { return free, nil }
	s, _ := Open(opts)
	b := Batch{Envelopes: []json.RawMessage{event("a")}, Cursor: func() error { return errors.New("checkpoint") }}
	if s.Append(b) == nil {
		t.Fatal("expected checkpoint failure")
	}
	free = 0
	b.Cursor = func() error { return nil }
	if err := s.Append(b); err != nil {
		t.Fatal(err)
	}
}

func TestNewShardAllocationReserve(t *testing.T) {
	opts := options(t.TempDir())
	opts.Free = func(string) (int64, error) { return opts.Reserve + int64(len(event("a"))) + 2, nil }
	s, _ := Open(opts)
	if err := s.Append(Batch{Envelopes: []json.RawMessage{event("a")}}); !errors.Is(err, ErrFreeSpace) {
		t.Fatalf("reserve: %v", err)
	}
}

func TestAppendDoesNotReadHistory(t *testing.T) {
	s, _ := Open(options(t.TempDir()))
	if err := s.Append(Batch{Envelopes: []json.RawMessage{event("a")}}); err != nil {
		t.Fatal(err)
	}
	// An unrelated unreadable history entry must not be visited by Append.
	if err := os.Symlink("missing", filepath.Join(s.root, "unrelated")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.root, "events", "month=2026-11", "claude-acct-a.jsonl")
	if err := os.Rename(path, path+".retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("invalid history\n"), 0640); err != nil {
		t.Fatal(err)
	}
	raw := strings.Replace(string(event("b")), `"profile":"acct-a"`, `"profile":"acct-b"`, 1)
	if err := s.Append(Batch{Envelopes: []json.RawMessage{json.RawMessage(raw)}}); err != nil {
		t.Fatal(err)
	}
}

func TestTailRecoveryMemoryBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Allocation, rather than elapsed time, distinguishes streaming tail repair.
	const size = 32 << 20
	if err = f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteAt([]byte("\npartial"), size-8); err != nil {
		t.Fatal(err)
	}
	f.Close()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err = healTail(path); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if after.TotalAlloc-before.TotalAlloc > 1<<20 {
		t.Fatal("tail recovery loaded history into memory")
	}
	info, _ := os.Stat(path)
	if info.Size() != size-7 {
		t.Fatalf("size %d", info.Size())
	}
}
