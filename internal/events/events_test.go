package events

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func event(id string) []byte {
	return []byte(`{"schema_version":1,"kind":"usage","event_id":"` + id + `","source":"claude","parser_version":"v1","host":"node-a","profile":"acct-a","harness":"claude-code","source_schema_version":null,"provider":null,"observed_at":{"original":null,"epoch_seconds":null,"quality":"unknown"},"collected_at":{"original":null,"epoch_seconds":1,"quality":"collection"},"locator":{"type":"probe","probe_id":"probe-1","part":"usage"},"quality":{"status":"observed","reasons":[],"missing_fields":[]},"data":{}}`)
}

func options(root string) Options {
	return Options{Root: root, Budget: 1 << 20, Reserve: 128, Now: func() time.Time { return time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC) }, Free: func(string) (int64, error) { return 1 << 30, nil }}
}

func TestReplaySnapshotDoesNotHoldAppendLock(t *testing.T) {
	s, err := Open(options(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Batch{Envelopes: []json.RawMessage{event("first")}}); err != nil {
		t.Fatal(err)
	}
	count := 0
	err = s.Replay(func(json.RawMessage) error {
		count++
		if !s.mu.TryLock() {
			return errors.New("replay callback holds append lock")
		}
		s.mu.Unlock()
		return s.Append(Batch{Envelopes: []json.RawMessage{event("later")}})
	})
	if err != nil || count != 1 {
		t.Fatalf("snapshot count=%d err=%v", count, err)
	}
}

func TestReplaySourceSkipsUnrelatedShard(t *testing.T) {
	root := t.TempDir()
	s, err := Open(options(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Batch{Envelopes: []json.RawMessage{event("first")}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "events", "month=2026-11", "unrelated.jsonl"), []byte("invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	count := 0
	err = s.ReplaySource("claude", "acct-a", func(json.RawMessage) error { count++; return nil })
	if err != nil || count != 1 {
		t.Fatalf("scoped count=%d err=%v", count, err)
	}
}

func TestAppendSyncBeforeCursorAndReplayAfterCrash(t *testing.T) {
	root := t.TempDir()
	opts := options(root)
	opts.Sync = func(*os.File) error { return errors.New("disk failure") }
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	advanced := false
	if err := s.Append(Batch{Envelopes: []json.RawMessage{event("e1")}, Cursor: func() error { advanced = true; return nil }}); err == nil || advanced {
		t.Fatalf("append=%v cursor=%v, want failed append without cursor advance", err, advanced)
	}

	opts.Sync = nil
	s, err = Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Batch{Envelopes: []json.RawMessage{event("e1")}, Cursor: func() error { advanced = true; return nil }}); err != nil || !advanced {
		t.Fatalf("replay append=%v cursor=%v", err, advanced)
	}
	var got []string
	if err := s.Replay(func(raw json.RawMessage) error { got = append(got, string(raw)); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d records, want one idempotent acknowledged event", len(got))
	}
}

func TestTornTailHealedOnOpen(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "events", "month=2026-11")
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	shard := filepath.Join(path, "claude-acct-a.jsonl")
	if err := os.WriteFile(shard, append(append(event("ok"), '\n'), []byte(`{"schema_version":1`)...), 0o640); err != nil {
		t.Fatal(err)
	}
	s, err := Open(options(root))
	if err != nil {
		t.Fatal(err)
	}
	var got int
	if err := s.Replay(func(json.RawMessage) error { got++; return nil }); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(shard)
	if err != nil || got != 1 || !strings.HasSuffix(string(b), "\n") {
		t.Fatalf("tail not healed: records=%d bytes=%q err=%v", got, b, err)
	}
}

func TestBudgetRefusesThenResumesWithoutCorruption(t *testing.T) {
	root := t.TempDir()
	opts := options(root)
	available := int64(len(event("e1")) + 128)
	opts.Free = func(string) (int64, error) { return available, nil }
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Batch{Envelopes: []json.RawMessage{event("e1")}}); !errors.Is(err, ErrFreeSpace) {
		t.Fatalf("append=%v, want free-space error", err)
	}
	if err := s.Replay(func(json.RawMessage) error { t.Fatal("budget rejection wrote data"); return nil }); err != nil {
		t.Fatal(err)
	}
	available = 1 << 30 // Operator restores space; the next normal batch resumes without a restart.
	if err := s.Append(Batch{Envelopes: []json.RawMessage{event("e1")}}); err != nil {
		t.Fatal(err)
	}
	var got int
	if err := s.Replay(func(json.RawMessage) error { got++; return nil }); err != nil || got != 1 {
		t.Fatalf("recovery replay got=%d err=%v", got, err)
	}

	info, err := os.Stat(filepath.Join(root, "events", "month=2026-11", "claude-acct-a.jsonl"))
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("shard permissions=%v err=%v", info.Mode(), err)
	}
	dir, err := os.Stat(filepath.Join(root, "events", "month=2026-11"))
	if err != nil || dir.Mode().Perm() != 0o750 {
		t.Fatalf("directory permissions=%v err=%v", dir.Mode(), err)
	}
}
