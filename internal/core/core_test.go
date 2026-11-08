package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

type timedFreshnessSource struct {
	*fakeSource
	timeout time.Duration
}

type passiveFreshnessSource struct {
	*fakeSource
	at time.Time
}

func (s *passiveFreshnessSource) SampleObservedAt(sample Sample) time.Time {
	if strings.HasPrefix(sample.Name, "agent_quota_") {
		return s.at
	}
	return time.Time{}
}

func (s *timedFreshnessSource) ReadTimeout() time.Duration { return s.timeout }

func TestFreshnessTimeoutAndConfiguredFloor(t *testing.T) {
	src := &timedFreshnessSource{&fakeSource{interval: 120 * time.Second}, 40 * time.Second}
	if got := FreshnessLimit(src, time.Minute); got != 295*time.Second {
		t.Fatalf("timeout budget ignored: %s", got)
	}
	if got := FreshnessLimit(src, 10*time.Minute); got != 10*time.Minute {
		t.Fatalf("configured floor ignored: %s", got)
	}
}

func TestFreshnessBoundary(t *testing.T) {
	for _, interval := range []time.Duration{900 * time.Second, 120 * time.Second} {
		t.Run(interval.String(), func(t *testing.T) {
			c := newClock()
			src := &fakeSource{name: "claude-oauth-usage", interval: interval, fn: func(int, json.RawMessage) (Result, json.RawMessage, error) {
				return Result{Samples: []Sample{{"agent_sessions", []Label{{"state", "idle"}}, 1}}}, nil, nil
			}}
			s := NewScheduler([]Source{src}, openState(t, t.TempDir(), c), c, quietLog(), time.Minute)
			s.Tick(context.Background(), nil, false, true)
			boundary := 2*interval + 10*time.Second + 15*time.Second
			c.Add(boundary - time.Nanosecond)
			for _, step := range []struct {
				advance time.Duration
				fresh   bool
			}{{0, true}, {time.Nanosecond, true}, {time.Nanosecond, false}} {
				c.Add(step.advance)
				if got := strings.Contains(render(t, s, c), `agent_sessions{state="idle",env="t"} 1`); got != step.fresh {
					t.Errorf("age %s: fresh=%v, want %v", c.Now().Sub(time.Unix(1767225600, 0)), got, step.fresh)
				}
			}
		})
	}
}

func TestCachedPassiveQuotaUsesReadingAge(t *testing.T) {
	c := newClock()
	observed := c.Now().Add(-4 * time.Minute)
	src := &fakeSource{name: "passive", interval: 120 * time.Second, fn: func(int, json.RawMessage) (Result, json.RawMessage, error) {
		return Result{Samples: []Sample{
			{Name: "agent_quota_observed_timestamp_seconds", Value: float64(observed.Unix())},
			{Name: "agent_quota_utilization_ratio", Value: 0.5},
			{Name: "agent_requests_total", Value: 1},
		}}, nil, nil
	}}
	s := NewScheduler([]Source{&passiveFreshnessSource{src, observed}}, openState(t, t.TempDir(), c), c, quietLog(), time.Minute)
	s.Tick(context.Background(), nil, false, true)
	if !strings.Contains(render(t, s, c), "agent_quota_utilization_ratio") {
		t.Fatal("fresh passive quota missing")
	}
	c.Add(26 * time.Second)
	out := render(t, s, c)
	if strings.Contains(out, "\nagent_quota_utilization_ratio") || strings.Contains(out, "\nagent_quota_observed_timestamp_seconds") {
		t.Fatal("old quota renewed by recent file read")
	}
	if !strings.Contains(out, "agent_requests_total") {
		t.Fatal("quota expiry dropped fresh source counters")
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

// While the state cannot be written, vendor sources must not run (their attempt would not survive a restart);
// local sources keep working, and vendor sources resume once a save succeeds.
func TestUnwritableStatePausesVendorCalls(t *testing.T) {
	c := newClock()
	st := openState(t, t.TempDir(), c)
	local := &fakeSource{name: "l", interval: time.Minute}
	net := &fakeSource{name: "n", interval: 15 * time.Minute, network: true}
	s := NewScheduler([]Source{local, net}, st, c, quietLog(), 30*time.Minute)
	st.failSave = errors.New("read-only file system")
	s.Tick(context.Background(), nil, false, true)
	c.Add(2 * time.Minute)
	s.Tick(context.Background(), nil, false, true)
	if net.calls != 0 || local.calls != 2 {
		t.Fatalf("want net=0 local=2, got net=%d local=%d", net.calls, local.calls)
	}
	st.failSave = nil
	c.Add(2 * time.Minute)
	s.Tick(context.Background(), nil, false, true) // local run saves successfully → vendor allowed again
	s.Tick(context.Background(), nil, false, true)
	if net.calls != 1 {
		t.Fatalf("vendor source did not resume after state became writable: %d", net.calls)
	}
}

// A null instance entry is structurally invalid state: quarantine it instead of crashing at start.
func TestNullInstanceQuarantined(t *testing.T) {
	c := newClock()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"epoch":1,"instances":{"claude-code/p/n":null}}`), 0o600)
	st := openState(t, dir, c)
	if !st.Quarantined {
		t.Fatal("null entry not quarantined")
	}
	NewScheduler([]Source{&fakeSource{name: "n", interval: time.Minute, network: true}}, st, c, quietLog(), 30*time.Minute)
}

// The quarantine backoff is written to disk: a restart within the hour still does not call the vendor.
func TestQuarantineBackoffSurvivesRestart(t *testing.T) {
	c := newClock()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "state.json"), []byte("{nope"), 0o600)
	st := openState(t, dir, c)
	NewScheduler([]Source{&fakeSource{name: "n", interval: 15 * time.Minute, network: true}}, st, c, quietLog(), 30*time.Minute)
	st.Close()
	c.Add(10 * time.Minute)
	net := &fakeSource{name: "n", interval: 15 * time.Minute, network: true}
	NewScheduler([]Source{net}, openState(t, dir, c), c, quietLog(), 30*time.Minute).Tick(context.Background(), nil, false, true)
	if net.calls != 0 {
		t.Fatal("restart inside the quarantine hour called the vendor")
	}
}

// Host-wide spacing survives a restart: a second vendor source is not called right after the first one's
// persisted attempt.
func TestHostSpacingSurvivesRestart(t *testing.T) {
	c := newClock()
	dir := t.TempDir()
	a := &fakeSource{name: "a", interval: 15 * time.Minute, network: true}
	st := openState(t, dir, c)
	NewScheduler([]Source{a}, st, c, quietLog(), 30*time.Minute).Tick(context.Background(), nil, false, true)
	st.Close()
	c.Add(5 * time.Second)
	b := &fakeSource{name: "b", interval: 15 * time.Minute, network: true}
	a2 := &fakeSource{name: "a", interval: 15 * time.Minute, network: true}
	NewScheduler([]Source{a2, b}, openState(t, dir, c), c, quietLog(), 30*time.Minute).Tick(context.Background(), nil, false, true)
	if b.calls != 0 {
		t.Fatal("vendor called within the host spacing right after a restart")
	}
}

type groupedSource struct {
	*fakeSource
	group string
}

func (g groupedSource) NetGroup() string { return g.group }

// A 429 from a source in its own gate group (agy's client) must not pause the default vendor group (Claude's
// private endpoint), and the reverse: the lock that backoff guards is per vendor, not per host.
func TestRateLimitBackoffIsPerGateGroup(t *testing.T) {
	c := newClock()
	limited := groupedSource{&fakeSource{name: "agy", interval: 15 * time.Minute, network: true,
		fn: func(int, json.RawMessage) (Result, json.RawMessage, error) {
			return Result{}, nil, &SourceError{Reason: ReasonRateLimited, Backoff: time.Hour}
		}}, "agy"}
	vendor := &fakeSource{name: "claude", interval: 15 * time.Minute, network: true}
	s := NewScheduler([]Source{limited, vendor}, openState(t, t.TempDir(), c), c, quietLog(), 30*time.Minute)
	s.Tick(context.Background(), func(x Source) bool { return x == Source(limited) }, false, true)
	s.Tick(context.Background(), nil, false, true)
	if limited.calls != 1 || vendor.calls != 1 {
		t.Fatalf("want agy=1 claude=1 (separate groups), got agy=%d claude=%d", limited.calls, vendor.calls)
	}
	c.Add(30 * time.Minute)
	s.Tick(context.Background(), nil, false, true)
	if limited.calls != 1 || vendor.calls != 2 {
		t.Fatalf("after 30m: agy still backed off, claude due again; got agy=%d claude=%d", limited.calls, vendor.calls)
	}
}

// Two samples with the same name and labels would make the whole scrape invalid; the renderer keeps the first.
func TestRenderDropsDuplicateSeries(t *testing.T) {
	c := newClock()
	dup := []Sample{
		{Name: "agent_quota_utilization_ratio", Labels: []Label{{"window", "5h"}, {"scope", "other"}}, Value: 0.5},
		{Name: "agent_quota_utilization_ratio", Labels: []Label{{"window", "5h"}, {"scope", "other"}}, Value: 0.7},
	}
	src := &fakeSource{name: "x", interval: time.Minute,
		fn: func(int, json.RawMessage) (Result, json.RawMessage, error) { return Result{Samples: dup}, nil, nil }}
	s := NewScheduler([]Source{src}, openState(t, t.TempDir(), c), c, quietLog(), 30*time.Minute)
	s.Tick(context.Background(), nil, true, true)
	out := render(t, s, c)
	if n := strings.Count(out, `agent_quota_utilization_ratio{window="5h",scope="other"`); n != 1 {
		t.Fatalf("want 1 series, got %d:\n%s", n, out)
	}
}

// After a restart, a persisted backoff blocks the whole gate group only if it came from a 429: one agy profile's
// failed run (60 m) or tripped guard (24 h) must not pause every other agy profile, while a 429, the vendor's own
// lock, still does.
func TestRestoredBackoffBlocksGroupOnlyFor429(t *testing.T) {
	for _, tc := range []struct {
		reason      string
		wantBlocked bool
	}{{ReasonUpstream, false}, {ReasonOther, false}, {ReasonRateLimited, true}} {
		c := newClock()
		dir := t.TempDir()
		failing := groupedSource{&fakeSource{name: "a", interval: 15 * time.Minute, network: true,
			fn: func(int, json.RawMessage) (Result, json.RawMessage, error) {
				return Result{}, nil, &SourceError{Reason: tc.reason, Backoff: time.Hour}
			}}, "agy"}
		st := openState(t, dir, c)
		NewScheduler([]Source{failing}, st, c, quietLog(), 30*time.Minute).Tick(context.Background(), nil, false, true)
		st.Close()
		c.Add(time.Minute) // past the host spacing
		other := groupedSource{&fakeSource{name: "b", interval: 15 * time.Minute, network: true}, "agy"}
		again := groupedSource{&fakeSource{name: "a", interval: 15 * time.Minute, network: true}, "agy"}
		NewScheduler([]Source{again, other}, openState(t, dir, c), c, quietLog(), 30*time.Minute).
			Tick(context.Background(), nil, false, true)
		if blocked := other.calls == 0; blocked != tc.wantBlocked {
			t.Errorf("%s: other source in the group blocked=%v, want %v", tc.reason, blocked, tc.wantBlocked)
		}
		if again.calls != 0 {
			t.Errorf("%s: the failing source itself must stay backed off after restart", tc.reason)
		}
	}
}

// A v0.1 state file has no local_backoff flag; its backoff (only ever from a 429) keeps blocking the group.
func TestV01StateBackoffStillBlocksGroup(t *testing.T) {
	c := newClock()
	dir := t.TempDir()
	st := openState(t, dir, c)
	a := &fakeSource{name: "a", interval: 15 * time.Minute, network: true}
	s := NewScheduler([]Source{a}, st, c, quietLog(), 30*time.Minute)
	key := s.insts[0].key
	st.Put(key, InstState{LastAttempt: c.Now().Add(-time.Hour).Unix(), BackoffUntil: c.Now().Add(time.Hour).Unix()})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	st.Close()
	b := &fakeSource{name: "b", interval: 15 * time.Minute, network: true}
	NewScheduler([]Source{&fakeSource{name: "a", interval: 15 * time.Minute, network: true}, b}, openState(t, dir, c), c,
		quietLog(), 30*time.Minute).Tick(context.Background(), nil, false, true)
	if b.calls != 0 {
		t.Fatal("a v0.1 backoff must still block the default vendor group")
	}
}

// More due sources in a gate group than the spacing allows must take turns: the oldest attempt goes first, so
// the sources later in config order are not starved.
func TestGateGroupSourcesTakeTurns(t *testing.T) {
	c := newClock()
	var srcs []Source
	var all []*fakeSource
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		f := &fakeSource{name: n, interval: time.Minute, network: true}
		srcs = append(srcs, f)
		all = append(all, f)
	}
	s := NewScheduler(srcs, openState(t, t.TempDir(), c), c, quietLog(), 30*time.Minute)
	// A 30 s gap allows two reads per 1 min interval for six sources: 10 minutes allow about 20 reads.
	for i := 0; i < 600; i++ {
		s.Tick(context.Background(), nil, false, true)
		c.Add(time.Second)
	}
	for _, f := range all {
		if f.calls < 2 {
			t.Errorf("source %s read %d times in 10 minutes: starved", f.name, f.calls)
		}
	}
}
