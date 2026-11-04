package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// Fixed clocks. The fixture tree (fixtures/codex/home) holds events from 2026-11-03T10:00Z to 2026-11-04T09:10Z.
var (
	tStart = time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)  // before every fixture event
	tNow   = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) // after every fixture event
)

const (
	fixtureDay3 = "2026/11/03/rollout-2026-11-03T10-00-00-00000000-0000-4000-8000-000000000001.jsonl"
	fixtureDay4 = "2026/11/04/rollout-2026-11-04T09-00-00-00000000-0000-4000-8000-000000000002.jsonl"

	// The newest rate_limits reading in the tree: event D, 2026-11-04T09:05:00Z.
	observedD = 1793783100
	reset5hD  = 1793801100 // 2026-11-04T14:05:00Z
	reset7dD  = 1794268800 // 2026-11-10T00:00:00Z
)

// quotaD is what event D serves while both its windows are open.
var quotaD = map[string]float64{
	"agent_quota_utilization_ratio{window=5h,scope=all}":       0.40,
	"agent_quota_reset_timestamp_seconds{window=5h,scope=all}": reset5hD,
	"agent_quota_utilization_ratio{window=7d,scope=all}":       0.10,
	"agent_quota_reset_timestamp_seconds{window=7d,scope=all}": reset7dD,
	"agent_quota_credits_available{scope=all}":                 1,
	"agent_quota_credits_balance{scope=all}":                   7.25,
	"agent_quota_observed_timestamp_seconds{}":                 observedD,
}

var baseLabels = []core.Label{{Name: "host", Value: "node-a"}, {Name: "harness", Value: "codex"},
	{Name: "profile", Value: "cx"}, {Name: "account", Value: "acct-a1"}, {Name: "provider", Value: "openai"}}

type harness struct {
	t     *testing.T
	dir   string // CODEX_HOME
	src   *RolloutsSource
	st    json.RawMessage
	now   time.Time
	parse int // parse errors of the last collect
}

func newSource(dir string) *RolloutsSource {
	return &RolloutsSource{Host: "node-a", Alias: "cx", Account: "acct-a1", Dir: dir, MaxModels: 20,
		StaleAfter: 45 * time.Minute, Interval: time.Minute}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "home", "u", ".codex")
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, dir: dir, src: newSource(dir), now: tNow}
}

// restart drops the in-memory source and builds a new one from the persisted blob only.
func (h *harness) restart() { h.src = newSource(h.dir) }

func (h *harness) path(rel string) string { return filepath.Join(h.dir, "sessions", rel) }

// collect runs one collect and returns its samples keyed by name and the non-profile labels, after checking that
// every sample carries the profile labels and that no content reached a sample, the error or the state.
func (h *harness) collect() map[string]float64 {
	h.t.Helper()
	res, st, err := h.src.Collect(context.Background(), h.now, h.st)
	if err != nil {
		h.t.Fatal(err)
	}
	noCanary(h.t, res.Samples, nil, string(st))
	h.st = st
	h.parse = res.Errors[core.ReasonParse]
	return keyed(h.t, res.Samples)
}

func keyed(t *testing.T, ss []core.Sample) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, s := range ss {
		if len(s.Labels) < len(baseLabels) {
			t.Fatalf("%s: labels %v", s.Name, s.Labels)
		}
		for i, l := range baseLabels {
			if s.Labels[i] != l {
				t.Fatalf("%s: label %d = %v, want %v", s.Name, i, s.Labels[i], l)
			}
		}
		var kv []string
		for _, l := range s.Labels[len(baseLabels):] {
			kv = append(kv, l.Name+"="+l.Value)
		}
		k := s.Name + "{" + strings.Join(kv, ",") + "}"
		if _, dup := out[k]; dup {
			t.Fatalf("duplicate series %s", k)
		}
		out[k] = s.Value
	}
	return out
}

// noCanary fails if a sample label, the error text or any extra string carries a canary (any case).
func noCanary(t *testing.T, ss []core.Sample, err error, extra ...string) {
	t.Helper()
	var b strings.Builder
	for _, s := range ss {
		for _, l := range s.Labels {
			b.WriteString(l.Value + " ")
		}
	}
	if err != nil {
		b.WriteString(err.Error())
	}
	for _, e := range extra {
		b.WriteString(e)
	}
	if strings.Contains(strings.ToLower(b.String()), "canary") {
		t.Fatalf("canary leaked: %s", b.String())
	}
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "codex", rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// copyTree copies fixtures/codex/home/sessions into the profile.
func (h *harness) copyTree() {
	h.t.Helper()
	src := filepath.Join("..", "..", "fixtures", "codex", "home", "sessions")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		write(h.t, h.path(rel), b)
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) setMtime(rel string, at time.Time) {
	h.t.Helper()
	if err := os.Chtimes(h.path(rel), at, at); err != nil {
		h.t.Fatal(err)
	}
}

type tu struct{ in, cached, cw, out, rs int64 }

func (u tu) json() map[string]int64 {
	return map[string]int64{"input_tokens": u.in, "cached_input_tokens": u.cached, "cache_write_input_tokens": u.cw,
		"output_tokens": u.out, "reasoning_output_tokens": u.rs, "total_tokens": u.in + u.out}
}

// tokenLine is one token_count event line; rateLimits is raw JSON ("null" for none).
func tokenLine(ts time.Time, last, total tu, rateLimits string) []byte {
	info, _ := json.Marshal(map[string]any{"total_token_usage": total.json(), "last_token_usage": last.json()})
	return []byte(fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":%s,"rate_limits":%s}}`+"\n",
		ts.UTC().Format(time.RFC3339Nano), info, rateLimits))
}

func turnContext(ts time.Time, model string) []byte {
	return []byte(fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"cwd":"/root/CANARY-CONTENT-cwd","model":%q,"user_instructions":"CANARY-CONTENT"}}`+"\n",
		ts.UTC().Format(time.RFC3339Nano), model))
}

// limits builds a rate_limits object. A window is "used,minutes,resets_at" with "-" for a JSON null; credits is
// raw JSON or "" for null.
func limits(limitID string, primary, secondary, credits string) string {
	win := func(w string) string {
		if w == "" {
			return "null"
		}
		f := strings.Split(w, ",")
		for i := range f {
			if f[i] == "-" {
				f[i] = "null"
			}
		}
		return fmt.Sprintf(`{"used_percent":%s,"window_minutes":%s,"resets_at":%s}`, f[0], f[1], f[2])
	}
	if credits == "" {
		credits = "null"
	}
	id := "null"
	if limitID != "" {
		id = fmt.Sprintf("%q", limitID)
	}
	return fmt.Sprintf(`{"limit_id":%s,"limit_name":"CANARY-CONTENT","primary":%s,"secondary":%s,"credits":%s}`,
		id, win(primary), win(secondary), credits)
}

// only returns the entries of m whose key starts with one of the prefixes.
func only(m map[string]float64, prefixes ...string) map[string]float64 {
	out := map[string]float64{}
	for k, v := range m {
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				out[k] = v
			}
		}
	}
	return out
}

func counters(m map[string]float64) map[string]float64 {
	return only(m, "agent_tokens_total", "agent_requests_total")
}

func quota(m map[string]float64) map[string]float64 { return only(m, "agent_quota_") }

func equal(t *testing.T, what string, got, want map[string]float64) {
	t.Helper()
	ok := len(got) == len(want)
	for k, v := range want {
		if g, has := got[k]; !has || g != v {
			ok = false
		}
	}
	if !ok {
		t.Fatalf("%s:\n got  %v\n want %v", what, got, want)
	}
}

func tokens(model string, in, out, cr, cw, rs, req float64) map[string]float64 {
	p := "agent_tokens_total{model=" + model + ",type="
	return map[string]float64{p + "input}": in, p + "output}": out, p + "cache_read}": cr, p + "cache_write}": cw,
		p + "reasoning}": rs, "agent_requests_total{model=" + model + ",outcome=ok}": req}
}

func merge(ms ...map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}
