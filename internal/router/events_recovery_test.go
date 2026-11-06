package router

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/events"
)

func qevents(t *testing.T, s *Source) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := s.EventStore.Replay(func(raw json.RawMessage) error {
		var e map[string]any
		if err := json.Unmarshal(raw, &e); err != nil {
			return err
		}
		out = append(out, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
func qlast(t *testing.T, s *Source) map[string]any {
	t.Helper()
	es := qevents(t, s)
	return es[len(es)-1]["data"].(map[string]any)
}
func TestEmailIdentity(t *testing.T) {
	dir := t.TempDir()
	_, w := writer(t, dir, false)
	defer w.Close()
	execAll(t, w, `INSERT INTO providerConnections(id,provider,name,email,isActive,data) VALUES ('conn-a','claude','','acct@example.com',1,'{}')`)
	s := newSource(dir, func(c *config.Config) { c.Metrics.AccountLabel = "email" })
	s.EventStore = eventStore(t)
	_, st := run(t, s, t0, nil)
	insertUsage(t, w, 1, 1, "conn-a", "m")
	run(t, s, t0.Add(time.Second), st)
	for _, e := range qevents(t, s) {
		if e["kind"] == "usage" {
			a := e["data"].(map[string]any)["account"]
			if a != nil {
				t.Fatalf("unproven event account: %v", a)
			}
		}
	}
}
func TestRateLimit(t *testing.T) {
	dir := t.TempDir()
	_, w := writer(t, dir, false)
	defer w.Close()
	execAll(t, w, `INSERT INTO providerConnections(id,provider,name,isActive,data) VALUES ('conn-a','claude','work-a',1,'{"rateLimitedUntil":1793797200}')`)
	s := newSource(dir, nil)
	s.EventStore = eventStore(t)
	run(t, s, t0, nil)
	if qlast(t, s)["connections"].([]any)[0].(map[string]any)["rate_limited_until"] != float64(1793797200) {
		t.Fatal("lost rate limit")
	}
}
func TestRetry(t *testing.T) {
	for _, mode := range []string{"sync", "state-save"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			_, w := writer(t, dir, false)
			defer w.Close()
			s := newSource(dir, nil)
			fail := false
			root := t.TempDir()
			store, err := events.Open(events.Options{Root: root, Budget: 64 << 20, Reserve: 1, Free: func(string) (int64, error) { return 1 << 30, nil }, Sync: func(f *os.File) error {
				if fail && strings.HasSuffix(f.Name(), ".jsonl") {
					return errors.New("injected sync")
				}
				return f.Sync()
			}})
			if err != nil {
				t.Fatal(err)
			}
			s.EventStore = store
			_, st := run(t, s, t0, nil)
			insertUsage(t, w, 1, 1, c1, "m")
			fail = mode == "sync"
			_, next, _ := s.Collect(context.Background(), t0.Add(time.Second), st)
			fail = false
			if mode == "state-save" {
				s = newSource(dir, nil)
				s.EventStore = store
				next = st
			}
			if next == nil {
				next = st
			}
			run(t, s, t0.Add(2*time.Second), next)
			insertUsage(t, w, 2, 1, c1, "m")
			run(t, s, t0.Add(3*time.Second), next)
			n := 0
			for _, e := range qevents(t, s) {
				if e["kind"] == "usage" {
					n++
				}
			}
			if n != 2 {
				t.Fatalf("usage events=%d", n)
			}
		})
	}
}
func TestStorePressureMetrics(t *testing.T) {
	dir := t.TempDir()
	_, w := writer(t, dir, false)
	defer w.Close()
	s := newSource(dir, nil)
	free := int64(1 << 30)
	store, err := events.Open(events.Options{Root: t.TempDir(), Budget: 64 << 20, Reserve: 1, Free: func(string) (int64, error) { return free, nil }})
	if err != nil {
		t.Fatal(err)
	}
	s.EventStore = store
	_, st := run(t, s, t0, nil)
	free = 0
	insertUsage(t, w, 1, 1, c1, "m")
	res, next, err := s.Collect(context.Background(), t0.Add(time.Second), st)
	if len(res.Samples) == 0 || next == nil {
		t.Fatalf("metrics suppressed: samples=%d state=%t error=%v", len(res.Samples), next != nil, err)
	}
	for _, ok := range store.WriteOutcomes() {
		if ok {
			t.Fatal("store failure reported healthy")
		}
	}
	free = 1 << 30
	run(t, s, t0.Add(2*time.Second), next)
	n := 0
	for _, e := range qevents(t, s) {
		if e["kind"] == "usage" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("recovered events=%d", n)
	}
}

func TestBudgetMetricsAndRecovery(t *testing.T) {
	dir := t.TempDir()
	_, w := writer(t, dir, false)
	defer w.Close()
	s := newSource(dir, nil)
	_, st := run(t, s, t0, nil)
	root := t.TempDir()
	open := func(budget int64) *events.Store {
		store, err := events.Open(events.Options{Root: root, Budget: budget, Reserve: 1, Free: func(string) (int64, error) { return 1 << 30, nil }})
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	s.EventStore = open(1)
	insertUsage(t, w, 1, 1, c1, "m")
	res, next, err := s.Collect(context.Background(), t0, st)
	if err == nil || len(res.Samples) == 0 || next == nil {
		t.Fatalf("budget suppressed metrics: %v", err)
	}
	s.EventStore = open(64 << 20)
	run(t, s, t0.Add(time.Second), next)
	n := 0
	for _, e := range qevents(t, s) {
		if e["kind"] == "usage" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("budget recovery events=%d", n)
	}
}
func TestReusedRows(t *testing.T) {
	dir := t.TempDir()
	_, w := writer(t, dir, false)
	defer w.Close()
	s := newSource(dir, nil)
	s.EventStore = eventStore(t)
	_, st := run(t, s, t0, nil)
	insertUsage(t, w, 1, 1, c1, "m")
	_, st = run(t, s, t0, st)
	execAll(t, w, "DELETE FROM usageHistory")
	_, st = run(t, s, t0, st)
	insertUsage(t, w, 1, 1, c1, "m")
	run(t, s, t0, st)
	ids := map[any]bool{}
	n := 0
	for _, e := range qevents(t, s) {
		if e["kind"] == "usage" {
			n++
			ids[e["event_id"]] = true
		}
	}
	if n != 2 || len(ids) != 2 {
		t.Fatalf("reused rows: events=%d unique=%d", n, len(ids))
	}
}
func TestSparseBacklog(t *testing.T) {
	dir := t.TempDir()
	_, w := writer(t, dir, false)
	defer w.Close()
	s := newSource(dir, nil)
	s.EventStore = eventStore(t)
	_, st := run(t, s, t0, nil)
	insertUsage(t, w, 100, 1, c1, "m")
	run(t, s, t0, st)
	d := qlast(t, s)
	if d["completion"] != "complete" || d["budget_exhausted"] != false {
		t.Fatalf("phantom backlog: %v", d)
	}
}
func TestDuration(t *testing.T) {
	dir := t.TempDir()
	_, w := writer(t, dir, false)
	defer w.Close()
	s := newSource(dir, nil)
	s.EventStore = eventStore(t)
	run(t, s, t0, nil)
	if qlast(t, s)["duration_seconds"].(float64) <= 0 {
		t.Fatal("zero duration")
	}
}
func TestStartupCoverage(t *testing.T) {
	dir := t.TempDir()
	_, w := writer(t, dir, false)
	defer w.Close()
	insertUsage(t, w, 1, 1, c1, "m")
	s := newSource(dir, nil)
	s.EventStore = eventStore(t)
	run(t, s, t0, nil)
	d := qlast(t, s)
	if d["completion"] != "partial" {
		t.Fatalf("startup covered: %v", d)
	}
}
func TestOriginalHealth(t *testing.T) {
	dir := t.TempDir()
	_, w := writer(t, dir, false)
	defer w.Close()
	execAll(t, w, `INSERT INTO providerConnections(id,provider,name,isActive,data) VALUES ('conn-a','claude','work-a',1,'{"testStatus":"testing"}')`)
	s := newSource(dir, nil)
	s.EventStore = eventStore(t)
	run(t, s, t0, nil)
	c := qlast(t, s)["connections"].([]any)[0].(map[string]any)
	if c["connection_id"] != "conn-a" || c["test_status"] != "testing" {
		t.Fatalf("invented health: %v", c)
	}
}
func TestZeroCost(t *testing.T) {
	s := newSource(t.TempDir(), nil)
	var e map[string]any
	json.Unmarshal(s.usageEvent(1, "claude", "m", "conn-a", "ok", "claude", "m", "work-a", 0, 0, true, 0, t0), &e)
	if e["data"].(map[string]any)["reported_cost"] != float64(0) {
		t.Fatal("observed zero cost lost")
	}
}
func TestMalformedCoverage(t *testing.T) {
	dir := t.TempDir()
	_, w := writer(t, dir, false)
	defer w.Close()
	s := newSource(dir, nil)
	s.EventStore = eventStore(t)
	_, st := run(t, s, t0, nil)
	insertUsage(t, w, 1, 1, c1, "m")
	execAll(t, w, `UPDATE usageHistory SET promptTokens='broken'`)
	run(t, s, t0, st)
	d := qlast(t, s)
	if d["completion"] != "partial" || d["skip_count"] != float64(1) || d["rows_appended"] != float64(0) {
		t.Fatalf("malformed coverage: %v", d)
	}
}
func TestFirstErrorUnknown(t *testing.T) {
	s := newSource(t.TempDir(), nil)
	var e map[string]any
	json.Unmarshal(s.collectEvent("run-a", t0, t0, 0, 0, map[string]int{"parse": 1, "other": 1}, nil), &e)
	if e["data"].(map[string]any)["first_error_class"] != nil {
		t.Fatal("aggregate map cannot prove first error")
	}
}
