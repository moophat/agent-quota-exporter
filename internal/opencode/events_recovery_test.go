package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/events"
	"github.com/moophat/agent-quota-exporter/internal/sqlro"
)

func qstore(t *testing.T, fail *bool) *events.Store {
	t.Helper()
	s, err := events.Open(events.Options{Root: t.TempDir(), Budget: 1 << 30, Reserve: 1, Free: func(string) (int64, error) {
		if fail != nil && *fail {
			return 0, fmt.Errorf("injected capacity failure")
		}
		return 1 << 40, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func qrows(t *testing.T, s *events.Store, kind string) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := s.Replay(func(raw json.RawMessage) error {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		if m["kind"] == kind {
			out = append(out, m)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestRapidPolls(t *testing.T) {
	path, w := emptyDB(t, false)
	s := source(path, 20)
	s.Events = qstore(t, nil)
	r := run{}
	r.collect(t, s, t0)
	insert(t, w, "one", done("provider-a", "model-a", 1, 2, 0, 0, 0, 0))
	r.collect(t, s, t0.Add(30*time.Second))
	if got := len(qrows(t, s.Events, "usage")); got != 1 {
		t.Fatalf("rapid poll lost usage: %d", got)
	}
}
func TestRestartOldCursor(t *testing.T) {
	path, w := emptyDB(t, false)
	ev := qstore(t, nil)
	s := source(path, 20)
	s.Events = ev
	r := run{}
	r.collect(t, s, t0)
	old := append(json.RawMessage(nil), r.state...)
	insert(t, w, "one", done("provider-a", "model-a", 1, 2, 0, 0, 0, 0))
	r.collect(t, s, t0.Add(time.Minute))
	before := qrows(t, ev, "usage")
	insert(t, w, "two", done("provider-a", "model-a", 3, 4, 0, 0, 0, 0))
	s = source(path, 20)
	s.Events = ev
	r.state = old
	r.collect(t, s, t0.Add(2*time.Minute))
	after := qrows(t, ev, "usage")
	if len(after) != 2 {
		t.Fatalf("restart replay blocked new usage: %d", len(after))
	}
	a, _ := json.Marshal(before[0])
	b, _ := json.Marshal(after[0])
	if string(a) != string(b) {
		t.Fatal("original envelope changed")
	}
}
func TestStoreFailureRetry(t *testing.T) {
	path, w := emptyDB(t, false)
	fail := false
	ev := qstore(t, &fail)
	s := source(path, 20)
	s.Events = ev
	r := run{}
	r.collect(t, s, t0)
	fail = true
	insert(t, w, "one", done("provider-a", "model-a", 1, 2, 0, 0, 0, 0))
	got := r.collect(t, s, t0.Add(time.Minute))
	if got["agent_tokens_total{provider-a,model-a,input}"] != 1 {
		t.Fatal("event failure stopped metrics")
	}
	fail = false
	s = source(path, 20)
	s.Events = ev
	r.collect(t, s, t0.Add(2*time.Minute))
	if len(qrows(t, ev, "usage")) != 1 {
		t.Fatal("failed append permanently lost usage")
	}
}
func TestModelAt(t *testing.T) {
	qfield(t, "lane-1", "provider-a", "model@v1", func(t *testing.T, m map[string]any) {
		if m["data"].(map[string]any)["model"] != "model@v1" {
			t.Fatal("model lost")
		}
	})
}
func TestZeroPresence(t *testing.T) {
	qfield(t, "lane-1", "provider-a", "model-a", func(t *testing.T, m map[string]any) {
		for _, k := range []string{"raw_tokens", "tokens"} {
			if m["data"].(map[string]any)[k].(map[string]any)["input"] != float64(0) {
				t.Fatal("observed zero became unknown")
			}
		}
	})
}
func TestProviderOriginal(t *testing.T) {
	qfield(t, "lane-1", "Custom-Provider", "model-a", func(t *testing.T, m map[string]any) {
		if m["provider"] != "Custom-Provider" {
			t.Fatalf("provider normalized: %v", m["provider"])
		}
	})
}
func TestProfiles(t *testing.T) {
	for _, alias := range []string{"work profile@local", "../profile", "quote\"slash\\", "password=alias", "日本語"} {
		t.Run(alias, func(t *testing.T) {
			qfield(t, alias, "provider-a", "model-a", func(t *testing.T, m map[string]any) {
				if m["profile"] != alias {
					t.Fatal("profile changed")
				}
			})
		})
	}
}
func qfield(t *testing.T, alias, provider, model string, check func(*testing.T, map[string]any)) {
	t.Helper()
	path, w := emptyDB(t, false)
	s := source(path, 20)
	s.Alias = alias
	s.Events = qstore(t, nil)
	r := run{}
	r.collect(t, s, t0)
	insert(t, w, "one", done(provider, model, 0, 2, 0, 0, 0, 0))
	r.collect(t, s, t0.Add(time.Minute))
	rows := qrows(t, s.Events, "usage")
	if len(rows) != 1 {
		t.Fatalf("event rejected: %d", len(rows))
	}
	check(t, rows[0])
}
func TestGeneration(t *testing.T) {
	path, w := emptyDB(t, false)
	s := source(path, 20)
	ev := qstore(t, nil)
	s.Events = ev
	r := run{}
	r.collect(t, s, t0)
	insert(t, w, "one", done("provider-a", "model-a", 1, 2, 0, 0, 0, 0))
	r.collect(t, s, t0.Add(time.Minute))
	w.Close()
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	w = writer(t, path, false)
	defer w.Close()
	exec(t, w, "opencode.schema.sql")
	r.collect(t, s, t0.Add(2*time.Minute))
	insert(t, w, "one", done("provider-a", "model-a", 1, 2, 0, 0, 0, 0))
	s = source(path, 20)
	s.Events = ev
	r.collect(t, s, t0.Add(3*time.Minute))
	rows := qrows(t, ev, "usage")
	if len(rows) != 2 {
		t.Fatalf("replacement lost event: %d", len(rows))
	}
	a := rows[0]["locator"].(map[string]any)
	b := rows[1]["locator"].(map[string]any)
	if a["generation"] == b["generation"] || a["relative_path"] != dbFile {
		t.Fatalf("invalid generation/relative locator: %v %v", a, b)
	}
	collects := qrows(t, ev, "collect")
	gap := false
	for _, row := range collects {
		d := row["data"].(map[string]any)
		for _, reason := range d["omission_reasons"].([]any) {
			if reason == "database_replaced_coverage_gap" {
				gap = true
				if d["completion"] == "complete" {
					t.Fatal("replacement gap claims complete")
				}
			}
		}
	}
	if !gap {
		t.Fatal("replacement coverage gap not reported")
	}
}
func TestFailedReadCoverage(t *testing.T) {
	path, w := emptyDB(t, false)
	s := source(path, 20)
	s.Events = qstore(t, nil)
	r := run{}
	r.collect(t, s, t0)
	exec(t, w, "DROP TABLE message")
	_, _, err := s.Collect(context.Background(), t0.Add(time.Minute), r.state)
	if err == nil {
		t.Fatal("expected query failure")
	}
	rows := qrows(t, s.Events, "collect")
	last := rows[len(rows)-1]["data"].(map[string]any)
	if last["completion"] == "complete" || last["errors"] == nil {
		t.Fatalf("failed read claims coverage: %v", last)
	}
}
func TestBoundedBacklog(t *testing.T) {
	path, w := emptyDB(t, false)
	s := source(path, 20)
	s.Events = qstore(t, nil)
	r := run{}
	r.collect(t, s, t0)
	tx, err := w.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3*pageSize+1; i++ {
		if _, err := tx.Exec(`INSERT INTO message(id,session_id,time_created,time_updated,data) VALUES(?, 'ses_00000000000000000001',0,0,?)`, fmt.Sprint(i), done("provider-a", "model-a", 1, 2, 0, 0, 0, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	maxRetained, batches := 0, 0
	s.eventBatch = func(n int) {
		batches++
		if n > maxRetained {
			maxRetained = n
		}
	}
	r.collect(t, s, t0.Add(time.Minute))
	if maxRetained > pageSize || maxRetained == 0 || batches < 4 {
		t.Fatalf("retained=%d batches=%d, want at most %d over multiple batches", maxRetained, batches, pageSize)
	}
	if n := len(qrows(t, s.Events, "usage")); n != 3*pageSize+1 {
		t.Fatalf("backlog not streamed: %d", n)
	}
	r.collect(t, s, t0.Add(2*time.Minute))
	if len(qrows(t, s.Events, "usage")) != 3*pageSize+1 {
		t.Fatal("bounded backlog did not resume")
	}
}

func TestSyncFailureExactRestart(t *testing.T) {
	path, w := emptyDB(t, false)
	root := t.TempDir()
	fail := false
	opts := events.Options{Root: root, Budget: 1 << 30, Reserve: 1, Free: func(string) (int64, error) { return 1 << 40, nil }, Sync: func(f *os.File) error {
		if fail && strings.HasSuffix(f.Name(), ".jsonl") {
			return fmt.Errorf("injected shard sync")
		}
		return f.Sync()
	}}
	ev, err := events.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	s := source(path, 20)
	s.Events = ev
	r := run{}
	r.collect(t, s, t0)
	insert(t, w, "one", done("provider-a", "model-a", 1, 2, 0, 0, 0, 0))
	fail = true
	r.collect(t, s, t0.Add(time.Minute))
	original := qrows(t, ev, "usage")
	if len(original) != 1 {
		t.Fatal("expected written row before failed sync")
	}
	fail = false
	ev, err = events.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	insert(t, w, "two", done("provider-a", "model-a", 3, 4, 0, 0, 0, 0))
	s = source(path, 20)
	s.Events = ev
	r.collect(t, s, t0.Add(2*time.Minute))
	rows := qrows(t, ev, "usage")
	if len(rows) != 2 {
		t.Fatalf("retry lost or duplicated usage: %d", len(rows))
	}
	a, _ := json.Marshal(original[0])
	b, _ := json.Marshal(rows[0])
	if string(a) != string(b) {
		t.Fatal("retry changed original envelope")
	}
}

func TestAbsentInvalidBuckets(t *testing.T) {
	path, w := emptyDB(t, false)
	s := source(path, 20)
	s.Events = qstore(t, nil)
	r := run{}
	r.collect(t, s, t0)
	insert(t, w, "one", `{"role":"assistant","time":{"completed":1767225601000},"tokens":{"input":0,"output":-1,"reasoning":1.5}}`)
	r.collect(t, s, t0.Add(time.Minute))
	rows := qrows(t, s.Events, "usage")
	if len(rows) != 1 {
		t.Fatal("missing usage")
	}
	for _, key := range []string{"tokens", "raw_tokens"} {
		b := rows[0]["data"].(map[string]any)[key].(map[string]any)
		if b["input"] != float64(0) || b["output"] != nil || b["reasoning"] != nil || b["cache_read"] != nil {
			t.Fatalf("presence lost: %v", b)
		}
	}
}

func TestInitialCheckpointFailure(t *testing.T) {
	path, w := emptyDB(t, false)
	root := t.TempDir()
	fail := true
	ev, err := events.Open(events.Options{Root: root, Budget: 1 << 30, Reserve: 1, Free: func(string) (int64, error) { return 1 << 40, nil }, Sync: func(f *os.File) error {
		if fail && strings.Contains(f.Name(), ".cursor-") {
			return fmt.Errorf("checkpoint failure")
		}
		return f.Sync()
	}})
	if err != nil {
		t.Fatal(err)
	}
	s := source(path, 20)
	s.Events = ev
	r := run{}
	r.collect(t, s, t0)
	insert(t, w, "one", done("provider-a", "model-a", 1, 2, 0, 0, 0, 0))
	r.collect(t, s, t0.Add(time.Minute))
	fail = false
	s = source(path, 20)
	s.Events = ev
	r.collect(t, s, t0.Add(2*time.Minute))
	if len(qrows(t, ev, "usage")) != 1 {
		t.Fatal("initial checkpoint failure lost usage across recovery/restart")
	}
}

func TestCloseFailureCoverage(t *testing.T) {
	path, _ := emptyDB(t, false)
	s := source(path, 20)
	s.Events = qstore(t, nil)
	s.closeDB = func(db *sqlro.DB) error { _ = db.Close(); return fmt.Errorf("injected close failure") }
	_, _, err := s.Collect(context.Background(), t0, nil)
	if err == nil {
		t.Fatal("close failure hidden")
	}
	rows := qrows(t, s.Events, "collect")
	if len(rows) != 1 {
		t.Fatal("missing summary")
	}
	data := rows[0]["data"].(map[string]any)
	if data["completion"] != "failed" || data["errors"] != float64(1) {
		t.Fatalf("close failure claims complete: %v", data)
	}
}

func TestShrinkKeepsPendingGeneration(t *testing.T) {
	path, w := emptyDB(t, false)
	s := source(path, 20)
	s.Events = qstore(t, nil)
	r := run{}
	r.collect(t, s, t0)
	insert(t, w, "pending", streaming("provider-a", "model-a", 1))
	insert(t, w, "tail", done("provider-a", "model-a", 1, 2, 0, 0, 0, 0))
	r.collect(t, s, t0.Add(time.Minute))
	generation := s.st.Generation
	exec(t, w, "DELETE FROM message WHERE id='tail'")
	r.collect(t, s, t0.Add(2*time.Minute))
	if s.st.Generation != generation {
		t.Fatal("shrink changed pending observation generation")
	}
	update(t, w, "pending", done("provider-a", "model-a", 1, 2, 0, 0, 0, 0))
	r.collect(t, s, t0.Add(3*time.Minute))
	if len(qrows(t, s.Events, "usage")) != 2 {
		t.Fatal("pending completion duplicated or lost")
	}
}
