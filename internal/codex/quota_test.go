package codex

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// The first-start scan takes the newest reading by its event time, not by the order files are read in (mtime).
func TestQuotaNewestByEventTimeNotMtime(t *testing.T) {
	for _, newer := range []string{fixtureDay3, fixtureDay4} {
		h := newHarness(t)
		h.copyTree()
		h.setMtime(fixtureDay3, tNow.Add(-2*time.Hour))
		h.setMtime(fixtureDay4, tNow.Add(-2*time.Hour))
		h.setMtime(newer, tNow.Add(-time.Hour))
		equal(t, "quota, newest mtime "+newer, quota(h.collect()), quotaD)
	}
}

// The scan is bounded: only the newest files by mtime are searched.
func TestQuotaScanBounded(t *testing.T) {
	h := newHarness(t)
	write(t, h.path(fixtureDay3), fixture(t, "home/sessions/"+fixtureDay3)) // holds the only rate_limits
	h.setMtime(fixtureDay3, tNow.Add(-2*time.Hour))
	for i := 0; i < scanFiles; i++ {
		rel := fmt.Sprintf("2026/11/04/rollout-%02d.jsonl", i)
		write(t, h.path(rel), turnContext(tNow.Add(-time.Hour), "gpt-5-codex"))
		h.setMtime(rel, tNow.Add(-time.Hour))
	}
	if got := quota(h.collect()); len(got) != 0 {
		t.Fatalf("scan went past %d files: %v", scanFiles, got)
	}
}

// The reading found at first start is persisted and served after a restart, even with the files gone.
func TestQuotaServedAfterRestartFromState(t *testing.T) {
	h := newHarness(t)
	h.copyTree()
	equal(t, "first start", quota(h.collect()), quotaD)
	for _, rel := range []string{fixtureDay3, fixtureDay4} {
		if err := os.Remove(h.path(rel)); err != nil {
			t.Fatal(err)
		}
	}
	h.restart()
	h.now = tNow.Add(time.Hour)
	equal(t, "after restart", quota(h.collect()), quotaD)
}

// A window goes absent once its own reset passes; credits go with the longest window of their reading, and the
// observed timestamp goes with the last served value.
func TestQuotaWindowsExpireAtReset(t *testing.T) {
	h := newHarness(t)
	h.copyTree()
	h.collect()
	h.now = time.Unix(reset5hD, 0)
	want := merge(quotaD)
	delete(want, "agent_quota_utilization_ratio{window=5h,scope=all}")
	delete(want, "agent_quota_reset_timestamp_seconds{window=5h,scope=all}")
	equal(t, "5h passed", quota(h.collect()), want)
	h.now = time.Unix(reset7dD, 0)
	if got := quota(h.collect()); len(got) != 0 {
		t.Fatalf("all windows passed: %v", got)
	}
}

// A window without a reset time is served for stale_after from its reading.
func TestQuotaWindowWithoutResetUsesStaleAfter(t *testing.T) {
	h := newHarness(t)
	h.collect()
	at := tNow.Add(time.Minute)
	write(t, h.path("2026/11/04/rollout-q.jsonl"), tokenLine(at, tu{1, 0, 0, 0, 0}, tu{1, 0, 0, 0, 0}, limits("", "50,300,-", "", "")))
	h.now = at.Add(45 * time.Minute)
	equal(t, "inside stale_after", quota(h.collect()), map[string]float64{
		"agent_quota_utilization_ratio{window=5h,scope=all}": 0.5,
		"agent_quota_observed_timestamp_seconds{}":           float64(at.Unix()),
	})
	h.now = at.Add(46 * time.Minute)
	if got := quota(h.collect()); len(got) != 0 {
		t.Fatalf("past stale_after: %v", got)
	}
}

// Credits stay until a newer reading reports credits, at most until the longest window of their own reading
// resets; a reading with no window keeps them 7 days.
func TestCreditsRetentionAndExpiry(t *testing.T) {
	h := newHarness(t)
	h.collect()
	p := h.path("2026/11/04/rollout-cr.jsonl")
	r1 := tNow.Add(time.Minute)
	r1Week := r1.Add(3 * 24 * time.Hour).Unix() // R1's longest window
	write(t, p, tokenLine(r1, tu{1, 0, 0, 0, 0}, tu{1, 0, 0, 0, 0}, limits("codex",
		fmt.Sprintf("10,300,%d", r1.Add(time.Hour).Unix()), fmt.Sprintf("20,10080,%d", r1Week),
		`{"has_credits":true,"unlimited":false,"balance":"3"}`)))
	h.now = r1.Add(time.Second)
	got := quota(h.collect())
	if got["agent_quota_credits_available{scope=all}"] != 1 || got["agent_quota_credits_balance{scope=all}"] != 3 {
		t.Fatalf("R1 credits: %v", got)
	}

	// R2 reports no credits: its windows replace R1's, R1's credits stay.
	r2 := tNow.Add(2 * time.Minute)
	r2Week := r2.Add(6 * 24 * time.Hour).Unix()
	appendFile(t, p, tokenLine(r2, tu{1, 0, 0, 0, 0}, tu{2, 0, 0, 0, 0}, limits("codex",
		fmt.Sprintf("11,300,%d", r2.Add(time.Hour).Unix()), fmt.Sprintf("21,10080,%d", r2Week), "")))
	h.now = r2.Add(time.Second)
	got = quota(h.collect())
	if got["agent_quota_utilization_ratio{window=5h,scope=all}"] != 0.11 ||
		got["agent_quota_credits_available{scope=all}"] != 1 || got["agent_quota_credits_balance{scope=all}"] != 3 ||
		got["agent_quota_observed_timestamp_seconds{}"] != float64(r2.Unix()) {
		t.Fatalf("R2 without credits: %v", got)
	}

	// At R1's longest reset the credits go, while R2's 7d window is still served.
	h.now = time.Unix(r1Week, 0)
	got = quota(h.collect())
	if _, ok := got["agent_quota_credits_available{scope=all}"]; ok {
		t.Fatalf("credits past their reading's longest reset: %v", got)
	}
	if got["agent_quota_utilization_ratio{window=7d,scope=all}"] != 0.21 {
		t.Fatalf("R2 7d window lost: %v", got)
	}

	// A newer reading with credits replaces them; unlimited has no balance.
	r3 := h.now.Add(time.Minute)
	appendFile(t, p, tokenLine(r3, tu{1, 0, 0, 0, 0}, tu{3, 0, 0, 0, 0}, limits("codex", "", "",
		`{"has_credits":false,"unlimited":false,"balance":"0"}`)))
	h.now = r3.Add(time.Second)
	got = quota(h.collect())
	if v, ok := got["agent_quota_credits_available{scope=all}"]; !ok || v != 0 || got["agent_quota_credits_balance{scope=all}"] != 0 {
		t.Fatalf("R3 out of credits: %v", got)
	}
	r4 := h.now.Add(time.Minute)
	appendFile(t, p, tokenLine(r4, tu{1, 0, 0, 0, 0}, tu{4, 0, 0, 0, 0}, limits("codex", "", "",
		`{"has_credits":false,"unlimited":true,"balance":null}`)))
	h.now = r4.Add(time.Second)
	equal(t, "R4 unlimited, no window", quota(h.collect()), map[string]float64{
		"agent_quota_credits_available{scope=all}": 1,
		"agent_quota_observed_timestamp_seconds{}": float64(r4.Unix()),
	})
	h.now = r4.Add(7*24*time.Hour - time.Second)
	if len(quota(h.collect())) != 2 {
		t.Fatal("R4 credits gone before 7 days")
	}
	h.now = r4.Add(7 * 24 * time.Hour)
	if got := quota(h.collect()); len(got) != 0 {
		t.Fatalf("R4 credits past 7 days: %v", got)
	}
}

// The shipped single-line fixture (shape from a real node, values invented) maps as documented.
func TestShippedTokenCountFixture(t *testing.T) {
	h := newHarness(t)
	h.now = time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	h.collect()
	write(t, h.path("2026/11/04/rollout-f.jsonl"), fixture(t, "rollouts/rollout-token-count.jsonl"))
	h.now = time.Date(2026, 11, 4, 9, 30, 0, 0, time.UTC)
	// last_token_usage {in 20, cached 10, cw 0, out 5, reasoning 1}: input 10, output 4.
	equal(t, "fixture", h.collect(), merge(tokens("unknown", 10, 4, 10, 0, 1, 1), map[string]float64{
		"agent_quota_utilization_ratio{window=5h,scope=all}":       0.09,
		"agent_quota_reset_timestamp_seconds{window=5h,scope=all}": 1793800000,
		"agent_quota_utilization_ratio{window=7d,scope=all}":       0.01,
		"agent_quota_reset_timestamp_seconds{window=7d,scope=all}": 1794300000,
		"agent_quota_credits_available{scope=all}":                 0,
		"agent_quota_credits_balance{scope=all}":                   0,
		"agent_quota_observed_timestamp_seconds{}":                 float64(time.Date(2026, 11, 4, 9, 21, 22, 0, time.UTC).Unix()),
	}))
}

// Two windows that map to the same labels are one series: the higher utilization and the earlier reset.
func TestQuotaCollisionMerge(t *testing.T) {
	h := newHarness(t)
	h.collect()
	p := h.path("2026/11/04/rollout-m.jsonl")
	at := tNow.Add(time.Minute)
	write(t, p, tokenLine(at, tu{1, 0, 0, 0, 0}, tu{1, 0, 0, 0, 0}, limits("codex",
		fmt.Sprintf("20,300,%d", tNow.Add(2*time.Hour).Unix()), fmt.Sprintf("30,300,%d", tNow.Add(time.Hour).Unix()), "")))
	h.now = at.Add(time.Second)
	equal(t, "same window", quota(h.collect()), map[string]float64{
		"agent_quota_utilization_ratio{window=5h,scope=all}":       0.3,
		"agent_quota_reset_timestamp_seconds{window=5h,scope=all}": float64(tNow.Add(time.Hour).Unix()),
		"agent_quota_observed_timestamp_seconds{}":                 float64(at.Unix()),
	})
	// Unknown windows and an out-of-vocabulary limit id fold to "other" and still merge, in a scope of their own.
	at2 := at.Add(time.Minute)
	appendFile(t, p, tokenLine(at2, tu{1, 0, 0, 0, 0}, tu{2, 0, 0, 0, 0}, limits("Not A Valid Id", "5,-,-", "7,-,-", "")))
	h.now = at2.Add(time.Second)
	equal(t, "other", quota(h.collect()), map[string]float64{
		"agent_quota_utilization_ratio{window=5h,scope=all}":       0.3,
		"agent_quota_reset_timestamp_seconds{window=5h,scope=all}": float64(tNow.Add(time.Hour).Unix()),
		"agent_quota_utilization_ratio{window=other,scope=other}":  0.07,
		"agent_quota_observed_timestamp_seconds{}":                 float64(at2.Unix()),
	})
}

// Codex alternating between limit ids keeps each scope's own newest reading: a newer event of one limit never
// removes the other's windows or credits.
func TestQuotaAlternatingLimitIDsKeepBothScopes(t *testing.T) {
	h := newHarness(t)
	h.collect()
	p := h.path("2026/11/04/rollout-s.jsonl")
	reset := tNow.Add(3 * time.Hour).Unix()
	var lines []byte
	for i, c := range []struct {
		id   string
		used int
	}{{"codex", 10}, {"codex_mini", 50}, {"codex", 20}, {"codex_mini", 60}} {
		n := int64(i + 1)
		lines = append(lines, tokenLine(tNow.Add(time.Duration(n)*time.Minute), tu{1, 0, 0, 0, 0}, tu{n, 0, 0, 0, 0},
			limits(c.id, fmt.Sprintf("%d,300,%d", c.used, reset), "", `{"has_credits":true,"unlimited":false,"balance":"2"}`))...)
	}
	write(t, p, lines)
	h.now = tNow.Add(5 * time.Minute)
	equal(t, "both scopes", quota(h.collect()), map[string]float64{
		"agent_quota_utilization_ratio{window=5h,scope=all}":              0.2,
		"agent_quota_reset_timestamp_seconds{window=5h,scope=all}":        float64(reset),
		"agent_quota_utilization_ratio{window=5h,scope=codex_mini}":       0.6,
		"agent_quota_reset_timestamp_seconds{window=5h,scope=codex_mini}": float64(reset),
		"agent_quota_credits_available{scope=all}":                        1,
		"agent_quota_credits_balance{scope=all}":                          2,
		"agent_quota_credits_available{scope=codex_mini}":                 1,
		"agent_quota_credits_balance{scope=codex_mini}":                   2,
		"agent_quota_observed_timestamp_seconds{}":                        float64(tNow.Add(4 * time.Minute).Unix()),
	})
	// The same holds for the first-start scan of history.
	h.restart()
	h.st = nil
	h.now = tNow.Add(6 * time.Minute)
	got := quota(h.collect())
	if got["agent_quota_utilization_ratio{window=5h,scope=all}"] != 0.2 || got["agent_quota_utilization_ratio{window=5h,scope=codex_mini}"] != 0.6 {
		t.Fatalf("scan lost a scope: %v", got)
	}
}

// At most maxScopes readings are kept per profile; a new scope beyond that evicts the scope whose newest reading
// is oldest.
func TestQuotaScopeCapEvictsOldest(t *testing.T) {
	h := newHarness(t)
	h.collect()
	reset := tNow.Add(3 * time.Hour).Unix()
	var lines []byte
	// Scope s0 is read last in the file but its event is the oldest; s1..s8 are newer.
	order := []int{1, 2, 3, 4, 5, 6, 7, 0, 8}
	for i, k := range order {
		lines = append(lines, tokenLine(tNow.Add(time.Duration(k+1)*time.Minute), tu{1, 0, 0, 0, 0}, tu{int64(i + 1), 0, 0, 0, 0},
			limits(fmt.Sprintf("s%d", k), fmt.Sprintf("%d,300,%d", k+1, reset), "", ""))...)
	}
	write(t, h.path("2026/11/04/rollout-cap.jsonl"), lines)
	h.now = tNow.Add(10 * time.Minute)
	got := quota(h.collect())
	if h.parse != 0 {
		t.Fatalf("eviction counted as an error: %d", h.parse)
	}
	for k := 0; k <= 8; k++ {
		_, ok := got[fmt.Sprintf("agent_quota_utilization_ratio{window=5h,scope=s%d}", k)]
		if ok != (k != 0) {
			t.Fatalf("scope s%d served=%v, want only s0 (oldest reading) evicted: %v", k, ok, got)
		}
	}
	if len(h.src.st.Quotas) != maxScopes {
		t.Fatalf("readings kept: %d", len(h.src.st.Quotas))
	}
}

// The observed timestamp (no scope label) is the newest reading among the scopes still served: a newer scope
// whose windows have all reset no longer counts.
func TestQuotaObservedIsNewestServedScope(t *testing.T) {
	h := newHarness(t)
	h.collect()
	old, recent := tNow.Add(time.Minute), tNow.Add(2*time.Minute)
	longReset, shortReset := tNow.Add(5*time.Hour).Unix(), tNow.Add(time.Hour).Unix()
	write(t, h.path("2026/11/04/rollout-o.jsonl"), append(
		tokenLine(old, tu{1, 0, 0, 0, 0}, tu{1, 0, 0, 0, 0}, limits("codex", fmt.Sprintf("10,300,%d", longReset), "", "")),
		tokenLine(recent, tu{1, 0, 0, 0, 0}, tu{2, 0, 0, 0, 0}, limits("codex_mini", fmt.Sprintf("30,60,%d", shortReset), "", ""))...))
	h.now = tNow.Add(3 * time.Minute)
	if got := quota(h.collect()); got["agent_quota_observed_timestamp_seconds{}"] != float64(recent.Unix()) {
		t.Fatalf("both served: %v", got)
	}
	h.now = time.Unix(shortReset, 0)
	got := quota(h.collect())
	if got["agent_quota_observed_timestamp_seconds{}"] != float64(old.Unix()) {
		t.Fatalf("after codex_mini reset, observed must fall back to the served scope: %v", got)
	}
	if _, ok := got["agent_quota_utilization_ratio{window=1h,scope=codex_mini}"]; ok {
		t.Fatalf("expired scope served: %v", got)
	}
}

func TestWindowAndScopeLabels(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	for _, c := range []struct {
		in   *float64
		want string
	}{{f(300), "5h"}, {f(10080), "7d"}, {f(1440), "1d"}, {f(43200), "30d"}, {f(120), "2h"}, {f(90), "90m"},
		{f(0), "other"}, {f(-5), "other"}, {f(7.5), "other"}, {nil, "other"}} {
		if got := windowLabel(c.in); got != c.want {
			t.Errorf("windowLabel(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	s := func(v string) *string { return &v }
	for _, c := range []struct {
		in   *string
		want string
	}{{nil, "all"}, {s(""), "all"}, {s("codex"), "all"}, {s("codex_mini-2"), "codex_mini-2"}, {s("Codex"), "other"},
		{s("a b"), "other"}, {s(strings.Repeat("x", 32)), strings.Repeat("x", 32)}, {s(strings.Repeat("x", 33)), "other"}} {
		if got := scopeLabel(c.in); got != c.want {
			t.Errorf("scopeLabel = %q, want %q", got, c.want)
		}
	}
}

// A resets_at no Codex would write (beyond int64 as a float, negative after conversion, or before 2000) is treated
// as absent: the window falls back to stale_after instead of being served forever.
func TestQuotaImplausibleResetFallsBackToStaleAfter(t *testing.T) {
	for _, reset := range []string{"1e300", "9.3e18", "-5", "12345"} {
		h := newHarness(t)
		h.collect()
		at := tNow.Add(time.Minute)
		write(t, h.path("2026/11/04/rollout-q.jsonl"), tokenLine(at, tu{1, 0, 0, 0, 0}, tu{1, 0, 0, 0, 0}, limits("", "50,300,"+reset, "", "")))
		h.now = at.Add(46 * time.Minute)
		if got := quota(h.collect()); len(got) != 0 {
			t.Errorf("resets_at %s: still served past stale_after: %v", reset, got)
		}
	}
}
