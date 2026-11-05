package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
)

// The tokens below are synthetic. "canary" in them is how a leak into a sample, error or state blob is caught.
const refreshCanary = "canary-refresh-token-must-never-be-read"

const usageBody = `{"plan_type":"plus",
 "rate_limit":{"limit_reached":false,
  "primary_window":{"used_percent":40,"limit_window_seconds":18000,"reset_after_seconds":600,"reset_at":1793801100},
  "secondary_window":{"used_percent":10.0,"limit_window_seconds":604800,"reset_at":1794268800}},
 "code_review_rate_limit":{"primary_window":{"used_percent":5,"limit_window_seconds":604800,"reset_at":1794268800}},
 "credits":{"has_credits":true,"unlimited":false,"balance":"7.25"}}`

const creditsBody = `{"available_count":2,"credits":[
 {"status":"available","granted_at":"2026-10-01T00:00:00Z","expires_at":"2026-11-07T00:00:00Z"},
 {"status":"available","granted_at":"2026-10-02T00:00:00Z","expires_at":"2026-12-01T00:00:00Z"},
 {"status":"redeemed","granted_at":"2026-09-01T00:00:00Z","expires_at":"2026-11-05T00:00:00Z"}]}`

var (
	uNow    = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	expiry7 = time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC).Unix()
)

// goldenUsage is what usageBody and creditsBody serve at uNow. Same series, labels, window and scope mapping as
// the rollouts source (see quotaD), except that the observed time is the read time.
var goldenUsage = map[string]float64{
	"agent_quota_utilization_ratio{window=5h,scope=all}":               0.40,
	"agent_quota_reset_timestamp_seconds{window=5h,scope=all}":         reset5hD,
	"agent_quota_utilization_ratio{window=7d,scope=all}":               0.10,
	"agent_quota_reset_timestamp_seconds{window=7d,scope=all}":         reset7dD,
	"agent_quota_utilization_ratio{window=7d,scope=code_review}":       0.05,
	"agent_quota_reset_timestamp_seconds{window=7d,scope=code_review}": reset7dD,
	"agent_quota_credits_available{scope=all}":                         1,
	"agent_quota_credits_balance{scope=all}":                           7.25,
	"agent_quota_observed_timestamp_seconds{}":                         float64(uNow.Unix()),
	"agent_quota_reset_credits_available{}":                            2,
	"agent_quota_reset_credits_next_expiry_timestamp_seconds{}":        float64(expiry7),
}

func jwt(exp time.Time) string {
	seg := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return "canarycanarycanary." + seg(fmt.Sprintf(`{"exp":%d}`, exp.Unix())) + ".sig-sig-sig"
}

// fakeVendor is the only server a test talks to. It records every request and fails the test on any path other
// than the two read paths, on any method but GET, and on any body: nothing can refresh a token or spend a slot.
type fakeVendor struct {
	t       *testing.T
	mu      sync.Mutex
	srv     *httptest.Server
	usage   []int // status per call; last repeats
	credits []int
	ubody   string
	cbody   string
	paths   []string
	auths   []string
	accts   []string
	nUsage  int
	nCred   int
}

func newVendor(t *testing.T) *fakeVendor {
	f := &fakeVendor{t: t, usage: []int{200}, credits: []int{200}, ubody: usageBody, cbody: creditsBody}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.accts = append(f.accts, r.Header.Get("ChatGPT-Account-Id"))
		if r.Method != http.MethodGet || r.ContentLength > 0 || strings.Contains(r.URL.Path, "consume") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var code int
		var body string
		switch r.URL.Path {
		case usagePath:
			code, body = f.usage[min(f.nUsage, len(f.usage)-1)], f.ubody
			f.nUsage++
		case creditsPath:
			code, body = f.credits[min(f.nCred, len(f.credits)-1)], f.cbody
			f.nCred++
		default:
			t.Errorf("request to %s: only the two read paths may be contacted (no refresh endpoint)", r.URL.Path)
			code = 404
		}
		w.WriteHeader(code)
		if code == 200 {
			w.Write([]byte(body))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// home writes <CODEX_HOME>/auth.json with an access token expiring at exp and a refresh token that must never be
// used, and returns the directory.
func home(t *testing.T, exp time.Time) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "home", "u", ".codex")
	writeAuth(t, dir, jwt(exp))
	return dir
}

func writeAuth(t *testing.T, dir, access string) {
	write(t, filepath.Join(dir, "auth.json"), []byte(fmt.Sprintf(
		`{"OPENAI_API_KEY":null,"tokens":{"id_token":"x","access_token":%q,"refresh_token":%q,"account_id":"acct-123"},"last_refresh":"2026-11-04T00:00:00Z"}`,
		access, refreshCanary)))
}

type guardRT struct {
	t    *testing.T
	host string
}

func (g guardRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != g.host {
		g.t.Errorf("request to %s: only the fake vendor may be contacted", r.URL.Host)
		return nil, fmt.Errorf("blocked")
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (f *fakeVendor) source(dir string) *UsageSource {
	u, _ := url.Parse(f.srv.URL)
	return &UsageSource{Host: "node-a", Alias: "cx", Account: "acct-a1", Dir: dir, StaleAfter: 45 * time.Minute,
		Interval: 3 * time.Minute, Backoff429: 10 * time.Minute, Version: "test", BaseURL: f.srv.URL,
		Transport: guardRT{f.t, u.Host}}
}

func collectU(t *testing.T, s *UsageSource, now time.Time, st json.RawMessage) (core.Result, json.RawMessage, error) {
	t.Helper()
	res, blob, err := s.Collect(context.Background(), now, st)
	noCanary(t, res.Samples, err, string(blob))
	return res, blob, err
}

func mustOK(t *testing.T, s *UsageSource, now time.Time, st json.RawMessage) (core.Result, json.RawMessage) {
	t.Helper()
	res, blob, err := collectU(t, s, now, st)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return res, blob
}

func reason(err error) string {
	if se, ok := err.(*core.SourceError); ok {
		return se.Reason
	}
	return fmt.Sprint(err)
}

func TestUsageParsesGolden(t *testing.T) {
	v := newVendor(t)
	s := v.source(home(t, uNow.Add(time.Hour)))
	res, _ := mustOK(t, s, uNow, nil)
	equal(t, "series", keyed(t, res.Samples), goldenUsage)
	if !res.ObservedAt.Equal(uNow) {
		t.Errorf("observed %v, want the read time %v", res.ObservedAt, uNow)
	}
	if got := strings.Join(v.paths, ","); got != "GET "+usagePath+",GET "+creditsPath {
		t.Errorf("requests: %s", got)
	}
	if v.accts[0] != "acct-123" || !strings.HasPrefix(v.auths[0], "Bearer canarycanarycanary.") {
		t.Errorf("auth headers: %q %q", v.auths[0], v.accts[0])
	}
}

// Without limit_window_seconds the primary window is 5h and the secondary 7d; a limit id outside the main one
// keeps its scope; a window with no used_percent is absent, never zero.
func TestUsageWindowAndScopeMapping(t *testing.T) {
	v := newVendor(t)
	v.ubody = `{"rate_limit":{"primary_window":{"used_percent":20,"reset_at":1793801100},"secondary_window":{"used_percent":30}},
	 "rate_limits_by_limit_id":{"codex":{"primary_window":{"used_percent":99}},"spark":{"rate_limit":{"primary_window":{"used_percent":7,"window_minutes":60}}}},
	 "additional_rate_limits":[{"limit_name":"Bad Name!","primary_window":{"used_percent":1}}]}`
	v.cbody = `{"available_count":0}`
	res, _ := mustOK(t, v.source(home(t, uNow.Add(time.Hour))), uNow, nil)
	got := keyed(t, res.Samples)
	want := map[string]float64{
		"agent_quota_utilization_ratio{window=5h,scope=all}":       0.20, // the main limit wins over by_limit_id "codex"
		"agent_quota_reset_timestamp_seconds{window=5h,scope=all}": reset5hD,
		"agent_quota_utilization_ratio{window=7d,scope=all}":       0.30,
		"agent_quota_utilization_ratio{window=1h,scope=spark}":     0.07,
		"agent_quota_utilization_ratio{window=5h,scope=other}":     0.01,
		"agent_quota_observed_timestamp_seconds{}":                 float64(uNow.Unix()),
		"agent_quota_reset_credits_available{}":                    0,
	}
	equal(t, "series", got, want)
}

func TestUsageNeverCallsConsume(t *testing.T) {
	for _, p := range []string{usagePath, creditsPath} {
		if strings.Contains(strings.ToLower(p), "consume") || !strings.HasPrefix(p, "/backend-api/wham/") {
			t.Errorf("source URL %q", p)
		}
	}
	if !strings.HasPrefix(usageBase+usagePath, "https://chatgpt.com/backend-api/wham/usage") {
		t.Errorf("base %q", usageBase)
	}
	// every scenario below runs against fakeVendor, which fails the test on a "consume" path or a non-GET.
	v := newVendor(t)
	s := v.source(home(t, uNow.Add(time.Hour)))
	for i := 0; i < 3; i++ {
		collectU(t, s, uNow.Add(time.Duration(i)*5*time.Minute), nil)
	}
	for _, p := range v.paths {
		if !strings.HasPrefix(p, "GET ") || strings.Contains(p, "consume") {
			t.Errorf("request %q", p)
		}
	}
}

// An expired access token is never sent and never refreshed: the last reading keeps being served with its true
// observed time and the error is counted as auth_expired.
func TestUsageExpiredTokenSkipped(t *testing.T) {
	v := newVendor(t)
	s := v.source(home(t, uNow.Add(5*time.Minute)))
	_, blob := mustOK(t, s, uNow, nil)
	calls := len(v.paths)

	later := uNow.Add(10 * time.Minute) // token expired at +5m
	res, _, err := collectU(t, s, later, blob)
	if reason(err) != core.ReasonAuthExpired {
		t.Fatalf("err %v", err)
	}
	if len(v.paths) != calls {
		t.Fatalf("expired token was sent: %v", v.paths[calls:])
	}
	if !res.ObservedAt.Equal(uNow) {
		t.Errorf("observed %v, want the true read time %v", res.ObservedAt, uNow)
	}
	got := keyed(t, res.Samples)
	if got["agent_quota_utilization_ratio{window=5h,scope=all}"] != 0.40 || got["agent_quota_observed_timestamp_seconds{}"] != float64(uNow.Unix()) {
		t.Errorf("last reading not served with its own time: %v", got)
	}

	// no history at all: nothing to serve, still the clear reason
	res, _, err = collectU(t, v.source(home(t, uNow.Add(-time.Hour))), uNow, nil)
	if reason(err) != core.ReasonAuthExpired || len(res.Samples) != 0 {
		t.Fatalf("err %v samples %d", err, len(res.Samples))
	}
}

func TestUsage429BacksOff(t *testing.T) {
	v := newVendor(t)
	v.usage = []int{200, 429}
	s := v.source(home(t, uNow.Add(time.Hour)))
	_, blob := mustOK(t, s, uNow, nil)

	res, _, err := collectU(t, s, uNow.Add(3*time.Minute), blob)
	se, ok := err.(*core.SourceError)
	if !ok || se.Reason != core.ReasonRateLimited || se.Backoff != 10*time.Minute {
		t.Fatalf("err %v", err)
	}
	if keyed(t, res.Samples)["agent_quota_observed_timestamp_seconds{}"] != float64(uNow.Unix()) {
		t.Error("the last reading must keep its true time through a 429")
	}
	s.Backoff429 = 0 // the default applies, never a zero backoff
	v.usage = []int{429}
	if _, _, err = collectU(t, s, uNow, nil); err.(*core.SourceError).Backoff != defaultUsageBackoff {
		t.Errorf("default backoff: %v", err)
	}
}

// A 429 on the reset-slot read backs the source off too, while the fresh quota values are kept.
func TestUsageCreditsRateLimitedKeepsQuota(t *testing.T) {
	v := newVendor(t)
	v.credits = []int{429}
	res, _, err := collectU(t, v.source(home(t, uNow.Add(time.Hour))), uNow, nil)
	if reason(err) != core.ReasonRateLimited {
		t.Fatalf("err %v", err)
	}
	got := keyed(t, res.Samples)
	if got["agent_quota_utilization_ratio{window=5h,scope=all}"] != 0.40 || !res.ObservedAt.Equal(uNow) {
		t.Errorf("fresh quota lost: %v", got)
	}
	if _, ok := got["agent_quota_reset_credits_available{}"]; ok {
		t.Error("reset credits invented")
	}
}

func TestUsageTwo401sStopUntilAuthChanges(t *testing.T) {
	v := newVendor(t)
	v.usage = []int{401, 401, 200}
	dir := home(t, uNow.Add(time.Hour))
	s := v.source(dir)
	var st json.RawMessage
	for i := 0; i < 4; i++ {
		_, b, err := collectU(t, s, uNow, st)
		st = b
		if reason(err) != core.ReasonAuthExpired {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if v.nUsage != 2 {
		t.Fatalf("want 2 requests then silence, got %d", v.nUsage)
	}
	time.Sleep(10 * time.Millisecond)
	writeAuth(t, dir, jwt(uNow.Add(2*time.Hour))) // the real client logged in again
	mustOK(t, s, uNow, st)
	if v.nUsage != 3 {
		t.Fatalf("after auth change: %d requests", v.nUsage)
	}
}

// Reading must not touch auth.json and no request may carry the refresh token or go to a refresh endpoint
// (fakeVendor fails the test on any other path).
func TestUsageNoRefreshNoWrite(t *testing.T) {
	v := newVendor(t)
	dir := home(t, uNow.Add(5*time.Minute))
	path := filepath.Join(dir, "auth.json")
	before, _ := os.ReadFile(path)
	fi0, _ := os.Stat(path)
	if err := os.Chmod(dir, 0o555); err != nil { // a write or temp file next to auth.json would now fail loudly
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}

	s := v.source(dir)
	var st json.RawMessage
	for _, c := range []struct {
		at      time.Time
		usage   int
		credits int
	}{{uNow, 200, 200}, {uNow.Add(time.Minute), 401, 200}, {uNow.Add(2 * time.Minute), 429, 200},
		{uNow.Add(3 * time.Minute), 500, 500}, {uNow.Add(10 * time.Minute), 200, 200}} { // the last one: expired token
		v.usage, v.credits = []int{c.usage}, []int{c.credits}
		_, b, _ := collectU(t, s, c.at, st)
		st = b
	}
	after, _ := os.ReadFile(path)
	fi1, _ := os.Stat(path)
	if !bytes.Equal(before, after) || !fi0.ModTime().Equal(fi1.ModTime()) {
		t.Error("auth.json was modified")
	}
	for i, a := range v.auths {
		if strings.Contains(a, refreshCanary) {
			t.Errorf("request %d carried the refresh token", i)
		}
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("files appeared next to auth.json: %v", ents)
	}
	if strings.Contains(string(st), refreshCanary) || strings.Contains(string(st), "canarycanary") {
		t.Error("a token reached the persisted state")
	}
}

// With codex_usage on, rollouts keeps its counters and emits no quota series: no series comes from both.
func TestUsageNoDuplicatesBesideRollouts(t *testing.T) {
	v := newVendor(t)
	h := newHarness(t)
	writeAuth(t, h.dir, jwt(tNow.Add(time.Hour)))

	cfg := config.Default()
	r := config.Resolved{Harness: config.HarnessCodex, Alias: "cx", Account: "acct-a1", Dir: h.dir,
		Quota: config.Quota{"codex_usage": true}}
	srcs := Build(cfg, r, "node-a", "test")
	if len(srcs) != 2 {
		t.Fatalf("sources: %d", len(srcs))
	}
	roll, usage := srcs[0].(*RolloutsSource), srcs[1].(*UsageSource)
	usage.BaseURL, usage.Transport = v.srv.URL, nil

	if _, _, err := roll.Collect(context.Background(), tStart, nil); err != nil { // first collect starts at the files' ends
		t.Fatal(err)
	}
	h.copyTree() // new files are counted from their start
	rres, _, err := roll.Collect(context.Background(), tNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	ures, _, err := usage.Collect(context.Background(), tNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	if q := quota(keyed(t, rres.Samples)); len(q) != 0 {
		t.Errorf("rollouts still emits quota series beside codex-usage: %v", q)
	}
	if len(counters(keyed(t, rres.Samples))) == 0 {
		t.Error("rollouts lost its token counters")
	}
	seen := map[string]bool{}
	for _, s := range append(rres.Samples, ures.Samples...) {
		k := s.Name + fmt.Sprint(s.Labels)
		if seen[k] {
			t.Errorf("duplicate series %s", k)
		}
		seen[k] = true
	}
	// without the opt-in rollouts is unchanged
	r.Quota = nil
	srcs = Build(cfg, r, "node-a", "test")
	if len(srcs) != 1 || srcs[0].(*RolloutsSource).SkipQuota {
		t.Error("rollouts must keep quota without codex_usage")
	}
}

func TestUsageAbsentExpiry(t *testing.T) {
	for name, body := range map[string]string{
		"no expires_at":   `{"available_count":1,"credits":[{"status":"available"}]}`,
		"null expires_at": `{"available_count":1,"credits":[{"status":"available","expires_at":null}]}`,
		"only past":       `{"available_count":1,"credits":[{"status":"available","expires_at":"2026-01-01T00:00:00Z"}]}`,
		"only spent":      `{"available_count":0,"credits":[{"status":"consumed","expires_at":"2027-01-01T00:00:00Z"}]}`,
		"count only":      `{"available_count":1}`,
		"unparsable":      `{"available_count":1,"credits":[{"status":"available","expires_at":"soon"}]}`,
	} {
		v := newVendor(t)
		v.cbody = body
		res, _ := mustOK(t, v.source(home(t, uNow.Add(time.Hour))), uNow, nil)
		got := keyed(t, res.Samples)
		if _, ok := got["agent_quota_reset_credits_next_expiry_timestamp_seconds{}"]; ok {
			t.Errorf("%s: expiry series present, want none (never 0)", name)
		}
		want := 1.0
		if name == "only spent" {
			want = 0
		}
		if got["agent_quota_reset_credits_available{}"] != want {
			t.Errorf("%s: available %v want %v", name, got["agent_quota_reset_credits_available{}"], want)
		}
	}
}

// Slots listed without available_count are counted from the list.
func TestUsageResetCreditsCountFromList(t *testing.T) {
	v := newVendor(t)
	v.cbody = `{"credits":[{"status":"available","expires_at":1793793700},{"status":"used"},{"status":"available","expires_at":1793793900000}]}`
	res, _ := mustOK(t, v.source(home(t, uNow.Add(time.Hour))), uNow, nil)
	got := keyed(t, res.Samples)
	if got["agent_quota_reset_credits_available{}"] != 2 || got["agent_quota_reset_credits_next_expiry_timestamp_seconds{}"] != 1793793700 {
		t.Errorf("%v", got)
	}
}

func TestUsageMalformedBodies(t *testing.T) {
	for name, body := range map[string]string{
		"not json":           `<html>`,
		"empty object":       `{}`,
		"null":               `null`,
		"array":              `[]`,
		"no used_percent":    `{"rate_limit":{"primary_window":{"reset_at":1}}}`,
		"string percent":     `{"rate_limit":{"primary_window":{"used_percent":"high"}}}`,
		"windows not object": `{"rate_limit":{"primary_window":3}}`,
	} {
		v := newVendor(t)
		v.ubody = body
		res, _, err := collectU(t, v.source(home(t, uNow.Add(time.Hour))), uNow, nil)
		if reason(err) != core.ReasonParse || len(res.Samples) != 0 {
			t.Errorf("%s: err %v samples %d", name, err, len(res.Samples))
		}
		if v.nCred != 0 {
			t.Errorf("%s: reset slots read after a bad usage body", name)
		}
	}
	// A bad reset-slot body counts one parse error and costs no quota value.
	for name, body := range map[string]string{"not json": `nope`, "empty": `{}`, "wrong types": `{"available_count":"many"}`, "list not array": `{"credits":5}`} {
		v := newVendor(t)
		v.cbody = body
		res, _, err := collectU(t, v.source(home(t, uNow.Add(time.Hour))), uNow, nil)
		got := keyed(t, res.Samples)
		if err != nil || res.Errors[core.ReasonParse] != 1 || got["agent_quota_utilization_ratio{window=5h,scope=all}"] != 0.40 {
			t.Errorf("%s: err %v errors %v", name, err, res.Errors)
		}
		if _, ok := got["agent_quota_reset_credits_available{}"]; ok {
			t.Errorf("%s: reset credits invented", name)
		}
	}
}

// A good reset-slot read followed by a failing one keeps serving the earlier count, but only for stale_after.
func TestUsageResetCreditsKeptThroughFailure(t *testing.T) {
	v := newVendor(t)
	s := v.source(home(t, uNow.Add(3*time.Hour)))
	_, blob := mustOK(t, s, uNow, nil)
	v.credits = []int{500}
	res, _ := mustOK(t, s, uNow.Add(3*time.Minute), blob)
	if res.Errors[core.ReasonUpstream] != 1 || keyed(t, res.Samples)["agent_quota_reset_credits_available{}"] != 2 {
		t.Errorf("errors %v series %v", res.Errors, keyed(t, res.Samples))
	}
	v.usage = []int{500} // far past stale_after, usage failing too
	res, _, _ = collectU(t, s, uNow.Add(2*time.Hour), blob)
	if _, ok := keyed(t, res.Samples)["agent_quota_reset_credits_available{}"]; ok {
		t.Error("stale reset credits still served")
	}
}

func TestUsageBadAuthFileNeverSent(t *testing.T) {
	for name, content := range map[string]string{
		"not json":         `{`,
		"api key login":    `{"OPENAI_API_KEY":"sk-canary-key-that-is-long-enough","tokens":null}`,
		"no tokens":        `{}`,
		"short token":      `{"tokens":{"access_token":"abc"}}`,
		"token with space": `{"tokens":{"access_token":"canary canary canary canary"}}`,
	} {
		v := newVendor(t)
		dir := filepath.Join(t.TempDir(), "home", "u", ".codex")
		write(t, filepath.Join(dir, "auth.json"), []byte(content))
		_, _, err := collectU(t, v.source(dir), uNow, nil)
		if err == nil || len(v.paths) != 0 {
			t.Errorf("%s: err %v calls %d", name, err, len(v.paths))
		}
	}
	v := newVendor(t) // missing file
	if _, _, err := collectU(t, v.source(t.TempDir()), uNow, nil); reason(err) != core.ReasonNotFound || len(v.paths) != 0 {
		t.Errorf("missing auth.json: %v", err)
	}
}

// A token without a readable exp is opaque and is tried; the server decides.
func TestUsageOpaqueTokenIsTried(t *testing.T) {
	v := newVendor(t)
	dir := filepath.Join(t.TempDir(), "home", "u", ".codex")
	writeAuth(t, dir, "canary-opaque-token-without-exp")
	mustOK(t, v.source(dir), uNow, nil)
	if v.nUsage != 1 {
		t.Errorf("calls %d", v.nUsage)
	}
}

func TestUsageRestoreServesLastReading(t *testing.T) {
	v := newVendor(t)
	dir := home(t, uNow.Add(time.Hour))
	_, blob := mustOK(t, v.source(dir), uNow, nil)
	res := v.source(dir).Restore(blob)
	if !res.ObservedAt.Equal(uNow) {
		t.Errorf("observed %v", res.ObservedAt)
	}
	equal(t, "restored", keyed(t, res.Samples), goldenUsage)
	if r := v.source(dir).Restore([]byte(`garbage`)); len(r.Samples) != 0 {
		t.Error("garbage state served")
	}
}

func TestUsageBuild(t *testing.T) {
	cfg := config.Default()
	r := config.Resolved{Harness: config.HarnessCodex, Alias: "cx", Dir: "/x", Quota: config.Quota{"codex_usage": true}}
	s := Build(cfg, r, "node-a", "1.2.3")[1].(*UsageSource)
	if s.Name() != "codex-usage" || s.Harness() != "codex" || s.Profile() != "cx" || !s.Network() || s.NetGroup() != "chatgpt" ||
		s.MinInterval() != 3*time.Minute || s.Backoff429 != 10*time.Minute || s.Account != "cx" || s.Version != "1.2.3" {
		t.Errorf("defaults: %+v", s)
	}
	cfg.Sources = map[string]config.Source{"codex-usage": {MinInterval: config.Duration(5 * time.Minute), Backoff429: config.Duration(time.Hour)}}
	s = Build(cfg, r, "node-a", "t")[1].(*UsageSource)
	if s.MinInterval() != 5*time.Minute || s.Backoff429 != time.Hour {
		t.Errorf("overrides: %+v", s)
	}
}

// fastClock is a manual clock.
type fastClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fastClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fastClock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// Many accounts must all be read within one interval: with the default 30 s gap twelve accounts would need six
// minutes per round, so the source's own spacing is what lets each be read every 3 minutes.
func TestUsageManyAccountsWithinOneInterval(t *testing.T) {
	v := newVendor(t)
	clk := &fastClock{t: uNow}
	var srcs []core.Source
	for i := 0; i < 12; i++ {
		s := v.source(home(t, uNow.Add(24*time.Hour)))
		s.Alias = fmt.Sprintf("cx%d", i)
		srcs = append(srcs, s)
	}
	state, err := core.OpenState(t.TempDir(), clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	sched := core.NewScheduler(srcs, state, clk, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), 45*time.Minute)
	for i := 0; i < 400; i++ { // 400 s
		sched.Tick(context.Background(), nil, false, true)
		clk.add(time.Second)
	}
	// 12 accounts, each read at least once, and every one at least twice in 400 s at a 3 min interval
	if v.nUsage < 12 || v.nUsage > 36 {
		t.Fatalf("usage reads in 400 s: %d", v.nUsage)
	}
	if v.nUsage < 24 {
		t.Fatalf("accounts starved by call spacing: %d reads in 400 s, want >= 24", v.nUsage)
	}
}
