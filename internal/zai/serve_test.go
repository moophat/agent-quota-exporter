package zai

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

var (
	testNow = time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	testID  = ident{Host: "node-a", Alias: "zai-main", Account: "acct-a1"}
)

func stateFrom(t *testing.T, fixture string, at time.Time) *state {
	t.Helper()
	p := mustParse(t, fixtureBytes(t, fixture))
	return &state{Reading: p.Reading, LastAt: at.UnixMilli()}
}

// byKey indexes samples as "name|window|scope" (window and scope empty when the series has none).
func byKey(ss []core.Sample) map[string]float64 {
	out := map[string]float64{}
	for _, s := range ss {
		var win, scope, plan string
		for _, l := range s.Labels {
			switch l.Name {
			case "window":
				win = l.Value
			case "scope":
				scope = l.Value
			case "plan":
				plan = l.Value
			}
		}
		k := s.Name + "|" + win + "|" + scope
		if plan != "" {
			k += "|plan=" + plan
		}
		out[k] = s.Value
	}
	return out
}

func TestServeGlobalFixture(t *testing.T) {
	ss := serve(testID, stateFrom(t, "quota-limit-global.json", testNow), time.Hour, testNow)
	got := byKey(ss)
	want := map[string]float64{
		"agent_quota_utilization_ratio|5h|all":                   0,
		"agent_quota_utilization_ratio|7d|all":                   0,
		"agent_quota_utilization_ratio|monthly|tool_calls":       0,
		"agent_quota_reset_timestamp_seconds|7d|all":             1767830400,
		"agent_quota_reset_timestamp_seconds|monthly|tool_calls": 1769904000,
		"agent_quota_used_calls|monthly|tool_calls":              0,
		"agent_quota_limit_calls|monthly|tool_calls":             100,
		"agent_quota_observed_timestamp_seconds||":               float64(testNow.Unix()),
		"agent_account_info|||plan=lite":                         1,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s: got %v ok=%v, want %v", k, g, ok, v)
		}
	}
}

func TestServeLabels(t *testing.T) {
	names := map[string]bool{}
	for _, n := range core.FamilyNames() {
		names[n] = true
	}
	for _, s := range serve(testID, stateFrom(t, "quota-limit-used.json", testNow), time.Hour, testNow) {
		if !names[s.Name] {
			t.Fatalf("%s is not a family", s.Name)
		}
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			t.Fatalf("%s non-finite", s.Name)
		}
		ls := map[string]string{}
		for _, l := range s.Labels {
			ls[l.Name] = l.Value
		}
		if ls["host"] != "node-a" || ls["harness"] != "zai" || ls["profile"] != "zai-main" || ls["account"] != "acct-a1" || ls["provider"] != "zai" {
			t.Fatalf("%s identity labels %v", s.Name, ls)
		}
		for k := range ls {
			switch k {
			case "host", "harness", "profile", "account", "provider", "window", "scope", "plan":
			default:
				t.Fatalf("%s: unexpected label %s", s.Name, k)
			}
		}
		if strings.HasSuffix(s.Name, "observed_timestamp_seconds") || s.Name == "agent_account_info" {
			if _, ok := ls["window"]; ok {
				t.Fatalf("%s must not carry window", s.Name)
			}
		}
	}
}

func TestServeFreshness(t *testing.T) {
	st := stateFrom(t, "quota-limit-global.json", testNow)
	if n := len(serve(testID, st, time.Hour, testNow.Add(time.Hour))); n == 0 {
		t.Fatal("reading at exactly the limit must be served")
	}
	if n := len(serve(testID, st, time.Hour, testNow.Add(time.Hour+time.Second))); n != 0 {
		t.Fatalf("stale reading served: %d samples", n)
	}
	if n := len(serve(testID, &state{}, time.Hour, testNow)); n != 0 {
		t.Fatal("empty state served")
	}
	if n := len(serve(testID, nil, time.Hour, testNow)); n != 0 {
		t.Fatal("nil state served")
	}
}

func TestServeDropsPassedWindows(t *testing.T) {
	st := stateFrom(t, "quota-limit-used.json", testNow)
	// the 5h window of this fixture resets at 2026-01-01T05:00Z
	got := byKey(serve(testID, st, 24*time.Hour, time.Date(2026, 1, 1, 5, 0, 1, 0, time.UTC)))
	if _, ok := got["agent_quota_utilization_ratio|5h|all"]; ok {
		t.Fatal("window past its reset still served")
	}
	if _, ok := got["agent_quota_utilization_ratio|7d|all"]; !ok {
		t.Fatal("window before its reset dropped")
	}
}

func TestServeAbsentIsNotZero(t *testing.T) {
	p := mustParse(t, []byte(`{"success":true,"data":{"level":"lite","limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5}]}}`))
	got := byKey(serve(testID, &state{Reading: p.Reading, LastAt: testNow.UnixMilli()}, time.Hour, testNow))
	for k := range got {
		if strings.HasPrefix(k, "agent_quota_utilization_ratio") || strings.HasPrefix(k, "agent_quota_used_calls") {
			t.Fatalf("invented %s", k)
		}
	}
	if _, ok := got["agent_account_info|||plan=lite"]; !ok {
		t.Fatalf("%v", got)
	}
}
