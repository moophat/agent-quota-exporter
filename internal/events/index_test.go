package events

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A warm restart must not parse historical envelopes again. Altering a historical
// row after its durable index checkpoint makes an accidental full replay visible.
func TestRestartUsesDurableIndex(t *testing.T) {
	root := t.TempDir()
	s, err := Open(Options{Root: root, Budget: 1 << 20, Reserve: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Batch{Envelopes: []json.RawMessage{event("indexed")}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(root, "events", "*", "*.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatal(paths, err)
	}
	f, err := os.OpenFile(paths[0], os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteAt([]byte("!"), 0); err != nil {
		t.Fatal(err)
	}
	f.Close()
	reopened, err := Open(Options{Root: root, Budget: 1 << 20, Reserve: 1})
	if err != nil {
		t.Fatalf("restart replayed indexed history: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
}
