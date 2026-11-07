package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/events"
)

func recoverySource(t *testing.T) (*tt, *events.Store) {
	t.Helper()
	x := newTT(t)
	store, err := events.Open(events.Options{Root: t.TempDir(), Budget: 1 << 30, Reserve: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	x.p.Events = store
	x.src.StateDir = t.TempDir()
	return x, store
}
func usageRows(t *testing.T, store *events.Store) int {
	t.Helper()
	n := 0
	if err := store.Replay(func(b json.RawMessage) error {
		var e events.Envelope
		json.Unmarshal(b, &e)
		if e.Kind == "usage" {
			n++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return n
}
func TestTranscriptRecoveryColdDeepRestartReplacementShrink(t *testing.T) {
	x, store := recoverySource(t)
	path := x.file(strings.Repeat("subagents/", 8) + "deep.jsonl")
	row := asstLine("deep", "model-x", "2026-01-01T00:00:00Z", 7)
	write(t, path, row)
	x.collect()
	if n := usageRows(t, store); n != 1 {
		t.Fatalf("cold deep rows=%d", n)
	}
	dir := x.src.StateDir
	x.restart()
	x.src.StateDir = dir
	x.collect()
	if n := usageRows(t, store); n != 1 {
		t.Fatalf("restart rows=%d", n)
	}
	replacement := path + ".new"
	write(t, replacement, row)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	x.collect()
	if n := usageRows(t, store); n != 2 {
		t.Fatalf("replacement physical rows=%d", n)
	}
	write(t, path, nil)
	x.collect()
	write(t, path, row)
	x.collect()
	if n := usageRows(t, store); n != 3 {
		t.Fatalf("shrink physical rows=%d", n)
	}
	if got := x.collect()["agent_tokens_total:model-x:input"]; got != 7 {
		t.Fatalf("dedupe tokens=%v", got)
	}
}
func TestTranscriptRecoveryBudgetTailResume(t *testing.T) {
	x, store := recoverySource(t)
	path := x.file("tail.jsonl")
	row := asstLine("one", "model-x", "2026-01-01T00:00:00Z", 1)
	row2 := asstLine("two", "model-x", "2026-01-01T00:00:00Z", 2)
	write(t, path, append(append([]byte{}, row...), row2[:30]...))
	x.src.ByteBudget = int64(len(row))
	x.collect()
	if n := usageRows(t, store); n != 1 {
		t.Fatalf("budget rows=%d", n)
	}
	x.src.ByteBudget = 256 << 20
	x.collect()
	if n := usageRows(t, store); n != 1 {
		t.Fatalf("tail rows=%d", n)
	}
	appendFile(t, path, row2[30:])
	x.collect()
	if n := usageRows(t, store); n != 2 {
		t.Fatalf("resume rows=%d", n)
	}
}
func TestTranscriptRecoveryOverlongStreamingResume(t *testing.T) {
	x, store := recoverySource(t)
	path := x.file("long.jsonl")
	write(t, path, nil)
	f, err := os.OpenFile(path, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	block := []byte(strings.Repeat("x", 1<<20))
	for i := 0; i < 65; i++ {
		if _, err = f.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	f.Write([]byte("\n"))
	f.Write(asstLine("after", "model-x", "2026-01-01T00:00:00Z", 1))
	f.Close()
	x.src.ByteBudget = 66 << 20
	x.collect()
	// Metric startup skips history; capture drains the overlong line in bounded chunks.
	for i := 0; i < 4 && usageRows(t, store) == 0; i++ {
		x.collect()
	}
	if n := usageRows(t, store); n != 1 {
		t.Fatalf("after overlong rows=%d", n)
	}
}
func TestTranscriptRecoveryWriteFailureDoesNotAdvance(t *testing.T) {
	x := newTT(t)
	fail := true
	store, err := events.Open(events.Options{Root: t.TempDir(), Budget: 1 << 30, Reserve: 1, Sync: func(f *os.File) error {
		if fail {
			return os.ErrPermission
		}
		return f.Sync()
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	x.p.Events = store
	x.src.StateDir = t.TempDir()
	write(t, x.file("a.jsonl"), asstLine("one", "model-x", "2026-01-01T00:00:00Z", 1))
	res, _, err := x.src.Collect(context.Background(), time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) == 0 {
		t.Fatal("missing durable write failure")
	}
	for _, c := range x.src.capture.Files {
		if c.Offset != 0 {
			t.Fatal("cursor advanced on failed fsync")
		}
	}
	fail = false
	x.collect()
	if n := usageRows(t, store); n != 1 {
		t.Fatalf("recovery rows=%d", n)
	}
	if _, err := os.Stat(filepath.Clean(x.src.capturePath())); err != nil {
		t.Fatal(err)
	}
}
