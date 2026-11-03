package core

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1767225600, 0).UTC()} }

type fakeSource struct {
	name     string
	interval time.Duration
	network  bool
	calls    int
	fn       func(n int, state json.RawMessage) (Result, json.RawMessage, error)
	mu       sync.Mutex
}

func (f *fakeSource) Name() string               { return f.name }
func (f *fakeSource) Harness() string            { return "claude-code" }
func (f *fakeSource) Profile() string            { return "p" }
func (f *fakeSource) MinInterval() time.Duration { return f.interval }
func (f *fakeSource) Network() bool              { return f.network }
func (f *fakeSource) Collect(_ context.Context, _ time.Time, st json.RawMessage) (Result, json.RawMessage, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(n, st)
	}
	return Result{}, nil, nil
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func openState(t *testing.T, dir string, c Clock) *State {
	t.Helper()
	st, err := OpenState(dir, c.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

// The vendor must be called at most once per interval no matter how often the scheduler ticks.
func TestIntervalRespectedUnderFastTicks(t *testing.T) {
	c := newClock()
	src := &fakeSource{name: "x", interval: 15 * time.Minute, network: true}
	s := NewScheduler([]Source{src}, openState(t, t.TempDir(), c), c, quietLog(), 30*time.Minute)
	for i := 0; i < 3600; i++ { // one hour at 1 s ticks
		s.Tick(context.Background(), nil, false, true)
		c.Add(time.Second)
	}
	// jitter adds at most 10 %: 0, ~15, ~30, ~45 minutes.
	if src.calls != 4 {
		t.Fatalf("want 4 calls in an hour, got %d", src.calls)
	}
}

// A 429 on one vendor source must silence every vendor source on the host (the lock is per IP).
func TestRateLimitBacksOffHostWide(t *testing.T) {
	c := newClock()
	a := &fakeSource{name: "a", interval: 15 * time.Minute, network: true,
		fn: func(int, json.RawMessage) (Result, json.RawMessage, error) {
			return Result{}, nil, &SourceError{Reason: ReasonRateLimited, Backoff: time.Hour}
		}}
	b := &fakeSource{name: "b", interval: 15 * time.Minute, network: true}
	s := NewScheduler([]Source{a, b}, openState(t, t.TempDir(), c), c, quietLog(), 30*time.Minute)
	s.Tick(context.Background(), func(x Source) bool { return x == a }, false, true)
	for i := 0; i < 59*60; i++ {
		s.Tick(context.Background(), nil, false, true)
		c.Add(time.Second)
	}
	if a.calls != 1 || b.calls != 0 {
		t.Fatalf("want a=1 b=0 within the hour, got a=%d b=%d", a.calls, b.calls)
	}
}

// Forcing (--once) runs local sources but never lets a network source skip its interval.
func TestForceNeverBypassesNetworkInterval(t *testing.T) {
	c := newClock()
	local := &fakeSource{name: "l", interval: time.Hour}
	net := &fakeSource{name: "n", interval: 15 * time.Minute, network: true}
	s := NewScheduler([]Source{local, net}, openState(t, t.TempDir(), c), c, quietLog(), 30*time.Minute)
	s.Tick(context.Background(), nil, true, true)
	c.Add(time.Minute)
	s.Tick(context.Background(), nil, true, true)
	if local.calls != 2 || net.calls != 1 {
		t.Fatalf("want local=2 net=1, got %d %d", local.calls, net.calls)
	}
}

// Gates survive a restart: a new process must not call the vendor again before the interval.
func TestAttemptPersistedAcrossRestart(t *testing.T) {
	c := newClock()
	dir := t.TempDir()
	src := &fakeSource{name: "n", interval: 15 * time.Minute, network: true}
	st := openState(t, dir, c)
	NewScheduler([]Source{src}, st, c, quietLog(), 30*time.Minute).Tick(context.Background(), nil, false, true)
	st.Close()
	c.Add(time.Minute)
	src2 := &fakeSource{name: "n", interval: 15 * time.Minute, network: true}
	s2 := NewScheduler([]Source{src2}, openState(t, dir, c), c, quietLog(), 30*time.Minute)
	s2.Tick(context.Background(), nil, false, true)
	if src2.calls != 0 {
		t.Fatal("restart re-called the vendor inside its interval")
	}
}

func TestPanicCountedAsOther(t *testing.T) {
	c := newClock()
	src := &fakeSource{name: "x", interval: time.Minute, fn: func(int, json.RawMessage) (Result, json.RawMessage, error) {
		panic("boom")
	}}
	s := NewScheduler([]Source{src}, openState(t, t.TempDir(), c), c, quietLog(), 30*time.Minute)
	s.Tick(context.Background(), nil, false, true)
	out := render(t, s, c)
	if !strings.Contains(out, `agent_source_errors_total{host="h",harness="claude-code",profile="p",source="x",reason="other",env="t"} 1`) ||
		!strings.Contains(out, `agent_source_up{host="h",harness="claude-code",profile="p",source="x",env="t"} 0`) {
		t.Fatal(out)
	}
}

func TestPluginStatePersists(t *testing.T) {
	c := newClock()
	dir := t.TempDir()
	src := &fakeSource{name: "x", interval: time.Minute, fn: func(n int, st json.RawMessage) (Result, json.RawMessage, error) {
		return Result{}, json.RawMessage(`{"n":1}`), nil
	}}
	st := openState(t, dir, c)
	NewScheduler([]Source{src}, st, c, quietLog(), 30*time.Minute).Tick(context.Background(), nil, false, true)
	st.Close()
	c.Add(2 * time.Minute)
	var got json.RawMessage
	src2 := &fakeSource{name: "x", interval: time.Minute, fn: func(n int, st json.RawMessage) (Result, json.RawMessage, error) {
		got = st
		return Result{}, nil, nil
	}}
	NewScheduler([]Source{src2}, openState(t, dir, c), c, quietLog(), 30*time.Minute).Tick(context.Background(), nil, false, true)
	if string(got) != `{"n":1}` {
		t.Fatalf("plugin state not restored: %s", got)
	}
	fi, _ := os.Stat(filepath.Join(dir, "state.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("state.json mode %v", fi.Mode().Perm())
	}
}

func TestSecondProcessRefused(t *testing.T) {
	c := newClock()
	dir := t.TempDir()
	openState(t, dir, c)
	if _, err := OpenState(dir, c.Now()); err == nil {
		t.Fatal("second OpenState on a locked dir must fail")
	}
}

// A corrupt state file is quarantined and network sources start backed off, so lost gates cannot hammer.
func TestCorruptStateQuarantined(t *testing.T) {
	c := newClock()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := openState(t, dir, c)
	if !st.Quarantined {
		t.Fatal("not quarantined")
	}
	m, _ := filepath.Glob(filepath.Join(dir, "state.json.corrupt-*"))
	if len(m) != 1 {
		t.Fatal("corrupt file not kept aside")
	}
	net := &fakeSource{name: "n", interval: 15 * time.Minute, network: true}
	NewScheduler([]Source{net}, st, c, quietLog(), 30*time.Minute).Tick(context.Background(), nil, false, true)
	if net.calls != 0 {
		t.Fatal("network source ran right after quarantine")
	}
}

// Values older than stale_after are dropped: absent, never zero.
func TestStaleDropped(t *testing.T) {
	c := newClock()
	src := &fakeSource{name: "x", interval: time.Minute, fn: func(n int, _ json.RawMessage) (Result, json.RawMessage, error) {
		if n > 1 {
			return Result{}, nil, Fail(ReasonNotFound)
		}
		return Result{Samples: []Sample{{"agent_sessions", []Label{{"state", "idle"}}, 1}}}, nil, nil
	}}
	s := NewScheduler([]Source{src}, openState(t, t.TempDir(), c), c, quietLog(), 30*time.Minute)
	s.Tick(context.Background(), nil, false, true)
	if !strings.Contains(render(t, s, c), `agent_sessions{state="idle",env="t"} 1`) {
		t.Fatal("fresh value missing")
	}
	c.Add(31 * time.Minute)
	s.Tick(context.Background(), nil, false, true)
	if strings.Contains(render(t, s, c), "agent_sessions") {
		t.Fatal("stale value still served")
	}
}

func TestRenderEscapingAndOrder(t *testing.T) {
	c := newClock()
	src := &fakeSource{name: "x", interval: time.Minute, fn: func(int, json.RawMessage) (Result, json.RawMessage, error) {
		return Result{Samples: []Sample{
			{"agent_tokens_total", []Label{{"model", "b"}}, 2},
			{"agent_tokens_total", []Label{{"model", `a"\` + "\n"}}, 1.5},
			{"agent_quota_utilization_ratio", []Label{{"window", "5h"}}, 0.42},
		}}, nil, nil
	}}
	s := NewScheduler([]Source{src}, openState(t, t.TempDir(), c), c, quietLog(), 30*time.Minute)
	s.Tick(context.Background(), nil, false, true)
	out := render(t, s, c)
	want := "agent_tokens_total{model=\"a\\\"\\\\\\n\",env=\"t\"} 1.5\nagent_tokens_total{model=\"b\",env=\"t\"} 2\n"
	if !strings.Contains(out, want) {
		t.Fatalf("escaping/order wrong:\n%s", out)
	}
	if strings.Index(out, "agent_quota_utilization_ratio") > strings.Index(out, "agent_tokens_total") {
		t.Fatal("family order not contract order")
	}
}

func TestHTTPReadOnly(t *testing.T) {
	c := newClock()
	s := NewScheduler(nil, openState(t, t.TempDir(), c), c, quietLog(), 30*time.Minute)
	e := &Exposition{Host: "h", Version: "test", Sched: s, Clock: c, ProcessStart: c.Now()}
	srv := httptest.NewServer(e.Handler())
	defer srv.Close()
	r, _ := http.Post(srv.URL+"/metrics", "text/plain", nil)
	if r.StatusCode != 405 {
		t.Fatalf("POST /metrics = %d", r.StatusCode)
	}
	r, _ = http.Get(srv.URL + "/metrics")
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !bytes.Contains(b, []byte(`agent_quota_exporter_build_info{version="test",contract="0"} 1`)) {
		t.Fatalf("GET /metrics = %d %s", r.StatusCode, b)
	}
	r, _ = http.Get(srv.URL + "/healthz")
	if r.StatusCode != 200 {
		t.Fatal("healthz")
	}
	r, _ = http.Get(srv.URL + "/nope")
	if r.StatusCode != 404 {
		t.Fatal("unknown path")
	}
}

func render(t *testing.T, s *Scheduler, c Clock) string {
	t.Helper()
	e := &Exposition{Host: "h", Static: []Label{{"env", "t"}}, Version: "test", Sched: s, Clock: c, ProcessStart: c.Now()}
	var b bytes.Buffer
	if err := e.Render(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
