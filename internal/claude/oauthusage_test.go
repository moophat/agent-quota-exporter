package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// stub is a fake vendor. Any request to another host or path fails the test: the plugin must never contact
// anything but the usage endpoint (in particular no token refresh endpoint).
type stub struct {
	t      *testing.T
	mu     sync.Mutex
	calls  int
	status []int // per call; last repeats
	body   []byte
	srv    *httptest.Server
	auth   []string
}

func newStub(t *testing.T, body []byte, status ...int) *stub {
	s := &stub{t: t, body: body, status: status}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.URL.Path != usagePath || r.Method != http.MethodGet || r.Header.Get("anthropic-beta") != betaHeader {
			t.Errorf("unexpected request %s %s beta=%q", r.Method, r.URL.Path, r.Header.Get("anthropic-beta"))
		}
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		code := 200
		if len(s.status) > 0 {
			code = s.status[min(s.calls, len(s.status)-1)]
		}
		s.calls++
		w.WriteHeader(code)
		if code == 200 {
			w.Write(s.body)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

type guardRT struct {
	t    *testing.T
	host string
}

func (g guardRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != g.host || r.URL.Path != usagePath {
		g.t.Errorf("request to %s%s: only the usage endpoint may be contacted", r.URL.Host, r.URL.Path)
		return nil, fmt.Errorf("blocked")
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (s *stub) source(p *Profile) *OAuthUsageSource {
	u, _ := url.Parse(s.srv.URL)
	return &OAuthUsageSource{P: p, Interval: 15 * time.Minute, Backoff429: time.Hour, Version: "test",
		Cache: NewQuotaCache(), Endpoint: s.srv.URL, Transport: guardRT{s.t, u.Host}}
}

func TestOAuthUsageParsesFixture(t *testing.T) {
	st := newStub(t, fixture(t, "oauth-usage-response.json"))
	p := newProfile(t, "a")
	res, _, err := collect(t, st.source(p), t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		window, scope string
		v             float64
	}{{"5h", "all", 0.42}, {"7d", "all", 0.185}, {"7d", "model_family_example", 0.07}} {
		if v, ok := find(res.Samples, "agent_quota_utilization_ratio", "window", c.window, "scope", c.scope, "provider", "anthropic"); !ok || v != c.v {
			t.Fatalf("%s/%s = %v %v", c.window, c.scope, v, ok)
		}
	}
	if v, _ := find(res.Samples, "agent_quota_reset_timestamp_seconds", "window", "5h"); v != 1767243600 {
		t.Fatalf("reset %v", v)
	}
	if _, ok := find(res.Samples, "agent_quota_limit_info", "window", "5h", "scope", "all", "severity", "normal"); !ok {
		t.Fatal("session limit missing")
	}
	if _, ok := find(res.Samples, "agent_quota_limit_info", "window", "7d", "scope", "all", "severity", "normal"); !ok {
		t.Fatal("weekly limit missing")
	}
	if v, _ := find(res.Samples, "agent_quota_observed_timestamp_seconds"); v != float64(t0.Unix()) {
		t.Fatal("observed timestamp")
	}
	if st.auth[0] != "Bearer CANARY-ACCESS-TOKEN-0000000000000000000000" {
		t.Fatal("token not sent as bearer")
	}
	noCanary(t, res.Samples, err)
}

// Credits and unknown keys are not quota windows; null windows are absent, never zero; duplicate limits collapse.
func TestOAuthUsageIgnoresNonWindowsAndNulls(t *testing.T) {
	body := []byte(`{"five_hour":{"utilization":10,"resets_at":null},"seven_day":null,"seven_day_opus":{"utilization":null},
	"extra_usage":{"is_enabled":true,"utilization":55},"seven_day_oauth_apps":null,
	"limits":[{"kind":"session","severity":"warning","is_active":true},{"kind":"session","severity":"normal","is_active":true},
	{"kind":"weekly_all","severity":"normal","is_active":false},{"kind":"mystery","severity":"normal","is_active":true}]}`)
	st := newStub(t, body)
	res, _, err := collect(t, st.source(newProfile(t, "a")), t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range res.Samples {
		if s.Name == "agent_quota_utilization_ratio" {
			n++
		}
		for _, l := range s.Labels {
			if l.Name == "window" && l.Value != "5h" && l.Value != "7d" {
				t.Fatalf("non-contract window %q", l.Value)
			}
		}
	}
	if n != 1 {
		t.Fatalf("want only five_hour, got %d ratio series", n)
	}
	if _, ok := find(res.Samples, "agent_quota_reset_timestamp_seconds"); ok {
		t.Fatal("null resets_at must be absent")
	}
	limits := 0
	for _, s := range res.Samples {
		if s.Name == "agent_quota_limit_info" {
			limits++
		}
	}
	if limits != 1 {
		t.Fatalf("want one deduplicated active limit, got %d", limits)
	}
}

// An expired token is never sent: refreshing is the real client's job, and a bad request counts toward the lock.
func TestOAuthUsageExpiredTokenNotSent(t *testing.T) {
	st := newStub(t, fixture(t, "oauth-usage-response.json"))
	_, _, err := collect(t, st.source(newProfile(t, "a")), t0.Add(3*time.Hour), nil)
	if err == nil || err.Error() != "source error: auth_expired" || st.calls != 0 {
		t.Fatalf("err %v calls %d", err, st.calls)
	}
}

func TestOAuthUsageMalformedTokenNotSent(t *testing.T) {
	st := newStub(t, fixture(t, "oauth-usage-response.json"))
	p := newProfile(t, "a")
	write(t, filepath.Join(p.Dir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"half written\n","expiresAt":1767232800000}}`))
	if _, _, err := collect(t, st.source(p), t0, nil); err == nil || st.calls != 0 {
		t.Fatalf("malformed token sent: err %v calls %d", err, st.calls)
	}
}

// Two consecutive 401s stop requests until the credential file changes.
func TestOAuthUsageStopsAfterTwo401(t *testing.T) {
	st := newStub(t, fixture(t, "oauth-usage-response.json"), 401, 401, 200)
	p := newProfile(t, "a")
	src := st.source(p)
	var state json.RawMessage
	for i := 0; i < 4; i++ {
		_, ns, err := collect(t, src, t0, state)
		state = ns
		if err == nil || err.Error() != "source error: auth_expired" {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if st.calls != 2 {
		t.Fatalf("want 2 requests then silence, got %d", st.calls)
	}
	time.Sleep(10 * time.Millisecond)
	write(t, filepath.Join(p.Dir, ".credentials.json"), fixture(t, "dot-credentials.json")) // client refreshed
	if _, _, err := collect(t, src, t0, state); err != nil || st.calls != 3 {
		t.Fatalf("after credential change: err %v calls %d", err, st.calls)
	}
}

func TestOAuthUsage429BacksOff(t *testing.T) {
	st := newStub(t, nil, 429)
	_, _, err := collect(t, st.source(newProfile(t, "a")), t0, nil)
	var se *core.SourceError
	if !errorsAs(err, &se) || se.Reason != "rate_limited" || se.Backoff != time.Hour {
		t.Fatalf("got %v", err)
	}
}

// Two profiles holding the same account cost one vendor call per interval.
func TestOAuthUsageOneCallPerAccount(t *testing.T) {
	st := newStub(t, fixture(t, "oauth-usage-response.json"))
	a, b := newProfile(t, "a"), newProfile(t, "b")
	sa := st.source(a)
	sb := st.source(b)
	sb.Cache = sa.Cache
	if _, _, err := collect(t, sa, t0, nil); err != nil {
		t.Fatal(err)
	}
	res, _, err := collect(t, sb, t0.Add(time.Minute), nil)
	if err != nil || st.calls != 1 {
		t.Fatalf("second profile of the same account called the vendor: err %v calls %d", err, st.calls)
	}
	if _, ok := find(res.Samples, "agent_quota_utilization_ratio", "profile", "b"); !ok {
		t.Fatal("shared answer not served under the second profile")
	}
	// Profiles whose account id is unknown never share an answer, not even with each other.
	for i, alias := range []string{"c", "d"} {
		c := newProfile(t, alias)
		os.Remove(filepath.Join(filepath.Dir(c.Dir), ".claude.json"))
		sc := st.source(c)
		sc.Cache = sa.Cache
		collect(t, sc, t0.Add(2*time.Minute), nil)
		if st.calls != 2+i {
			t.Fatalf("unknown account %s reused another answer: calls %d", alias, st.calls)
		}
	}
}

// After a restart the last good answer is served until it goes stale (no gap while the gate is closed).
func TestOAuthUsageRestoredAfterRestart(t *testing.T) {
	st := newStub(t, fixture(t, "oauth-usage-response.json"))
	p := newProfile(t, "a")
	_, state, err := collect(t, st.source(p), t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	res := st.source(p).Restore(state)
	if v, ok := find(res.Samples, "agent_quota_utilization_ratio", "window", "5h"); !ok || v != 0.42 || !res.ObservedAt.Equal(t0) {
		t.Fatalf("restore: %v %v %v", v, ok, res.ObservedAt)
	}
}

// Full path through the scheduler: an hour of 1 s ticks costs four vendor calls, and the token never shows up
// in /metrics or the logs.
func TestOAuthUsageThroughSchedulerNoLeak(t *testing.T) {
	st := newStub(t, fixture(t, "oauth-usage-response.json"))
	p := newProfile(t, "a")
	clk := &stepClock{t: t0}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	state, err := core.OpenState(t.TempDir(), clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	sched := core.NewScheduler([]core.Source{st.source(p)}, state, clk, log, 45*time.Minute)
	for i := 0; i < 3600; i++ {
		sched.Tick(context.Background(), nil, false, true)
		clk.add(time.Second)
	}
	if st.calls != 4 {
		t.Fatalf("want 4 vendor calls in an hour, got %d", st.calls)
	}
	var out bytes.Buffer
	(&core.Exposition{Host: "node-a", Version: "test", Sched: sched, Clock: clk, ProcessStart: t0}).Render(&out)
	if !strings.Contains(out.String(), `agent_quota_utilization_ratio{host="node-a",harness="claude-code",profile="a"`) {
		t.Fatal(out.String())
	}
	if strings.Contains(out.String()+logs.String(), "CANARY") {
		t.Fatal("canary leaked into /metrics or logs")
	}
	_ = io.Discard
}

type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *stepClock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
