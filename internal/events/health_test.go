package events

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestWriteOutcomeTracksDurability(t *testing.T) {
	for _, laterShard := range []bool{false, true} {
		t.Run(map[bool]string{false: "cursor", true: "later-shard"}[laterShard], func(t *testing.T) {
			opts := options(t.TempDir())
			opts.Sync = func(f *os.File) error {
				if laterShard && strings.HasSuffix(f.Name(), "other-acct-a.jsonl") {
					return errors.New("sync failed")
				}
				return f.Sync()
			}
			s, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			batch := Batch{Envelopes: []json.RawMessage{event("first")}, Cursor: func() error { return errors.New("checkpoint failed") }}
			if laterShard {
				batch.Envelopes = append(batch.Envelopes, []byte(strings.ReplaceAll(string(event("second")), `"source":"claude"`, `"source":"other"`)))
			}
			if s.Append(batch) == nil {
				t.Fatal("expected failure")
			}
			out := s.WriteOutcomes()
			if !out[Writer{"node-a", "claude-code", "acct-a", "claude"}] {
				t.Fatal("durable writer reported failed")
			}
			if laterShard && out[Writer{"node-a", "claude-code", "acct-a", "other"}] {
				t.Fatal("failed shard reported successful")
			}
		})
	}
}

func TestInvalidEventWriteOutcome(t *testing.T) {
	s, err := Open(options(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.ReplaceAll(string(event("invalid")), `"data":{}`, `"data":{"unexpected":true}`)
	if s.Append(Batch{Envelopes: []json.RawMessage{[]byte(raw)}}) == nil {
		t.Fatal("accepted invalid payload")
	}
	w := Writer{"node-a", "claude-code", "acct-a", "claude"}
	if ok, exists := s.WriteOutcomes()[w]; !exists || ok {
		t.Fatal("missing failed outcome for identifiable writer")
	}
	for _, raw := range []string{`{"host":"partial"`, strings.ReplaceAll(string(event("unsafe")), `"host":"node-a"`, `"host":"unsafe@example.com"`)} {
		_ = s.Append(Batch{Envelopes: []json.RawMessage{[]byte(raw)}})
	}
	if len(s.WriteOutcomes()) != 1 {
		t.Fatal("unsafe identity exposed")
	}
}

func TestStatsCapacityIOWithoutWriteLock(t *testing.T) {
	opts := options(t.TempDir())
	var s *Store
	opts.Free = func(string) (int64, error) {
		if !s.mu.TryLock() {
			return 0, errors.New("capacity measurement holds write lock")
		}
		s.mu.Unlock()
		return 1 << 30, nil
	}
	var err error
	s, err = Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	s.size = func(root string) (int64, error) {
		if !s.mu.TryLock() {
			return 0, errors.New("size measurement holds write lock")
		}
		s.mu.Unlock()
		return treeSize(root)
	}
	if _, err := s.Stats(); err != nil {
		t.Fatal(err)
	}
}
