package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// Successful cache reads require the root-owned writer; ordinary users cannot
// create a trusted fixture. The rejection tests below still run without root.
func newCacheProfile(t *testing.T) *Profile {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("creating a trusted root-owned cache needs root")
	}
	p := newProfile(t, "root")
	path := filepath.Join(p.Dir, ".credentials.json")
	at := t0.Add(-time.Hour)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return p
}

func cacheFile(t *testing.T, path, status string, at time.Time, body []byte) {
	t.Helper()
	b, err := json.Marshal(struct {
		Timestamp int64           `json:"timestamp"`
		Status    string          `json:"status"`
		Data      json.RawMessage `json:"data"`
	}{at.UnixMilli(), status, body})
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, b)
}

// The cache is the vendor body, not a second interpretation: all fields, nulls,
// scope collisions and the unknown-window cap must follow the endpoint parser.
func TestStatuslineCacheMappingAndReadOnly(t *testing.T) {
	bodies := [][]byte{
		fixture(t, "oauth-usage-response.json"),
		[]byte(`{"five_hour":{"utilization":12},"seven_day":null,"seven_day_opus":{"utilization":null},"extra_usage":{"utilization":20},"foo.bar":{"utilization":1},"foo_bar":{"utilization":2},"limits":[{"kind":"session","percent":99,"is_active":true,"severity":"warning"},{"kind":"weekly_x","percent":33}]}`),
	}
	var b strings.Builder
	b.WriteString(`{"five_hour":{"utilization":1}`)
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, `,"key_%02d":{"utilization":%d}`, i, i)
	}
	b.WriteString("}")
	bodies = append(bodies, []byte(b.String()))
	for i, body := range bodies {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			p := newCacheProfile(t)
			path := filepath.Join(t.TempDir(), "cache.json")
			at := t0.Add(-30*time.Second + 123*time.Millisecond)
			cacheFile(t, path, "available", at, body)
			before, _ := os.ReadFile(path)
			info, _ := os.Stat(path)
			if err := os.Chmod(path, 0444); err != nil {
				t.Fatal(err)
			}
			src := &StatuslineCacheSource{P: p, Path: path, Interval: time.Minute, StaleAfter: 45 * time.Minute}
			res, state, err := collect(t, src, t0, nil)
			if err != nil || state != nil || !res.ObservedAt.Equal(at) {
				t.Fatalf("result: %v %s %v", res.ObservedAt, state, err)
			}
			q, err := parseUsage(body)
			if err != nil {
				t.Fatal(err)
			}
			account, _ := p.Account()
			want := quotaSamples(p, account, q, at)
			if !reflect.DeepEqual(res.Samples, want) {
				t.Fatalf("cache differs from endpoint: %v / %v", res.Samples, want)
			}
			if v, ok := find(res.Samples, "agent_quota_observed_timestamp_seconds"); !ok || v != float64(at.Unix()) {
				t.Fatalf("observed %v %v", v, ok)
			}
			after, _ := os.ReadFile(path)
			newInfo, _ := os.Stat(path)
			entries, _ := os.ReadDir(filepath.Dir(path))
			if !bytes.Equal(before, after) || !os.SameFile(info, newInfo) || !info.ModTime().Equal(newInfo.ModTime()) || newInfo.Mode().Perm() != 0444 || len(entries) != 1 {
				t.Fatal("cache was modified or extra files created")
			}
		})
	}
}

func TestStatuslineCacheAbsent(t *testing.T) {
	for _, kind := range []string{"stale", "missing", "unavailable", "bad-json", "bad-data", "future", "zero-timestamp", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cache.json")
			src := &StatuslineCacheSource{P: newProfile(t, "root"), Path: path, Interval: time.Minute, StaleAfter: 45 * time.Minute}
			at, status, body := t0, "available", fixture(t, "oauth-usage-response.json")
			switch kind {
			case "stale":
				at = t0.Add(-45*time.Minute - time.Millisecond)
			case "unavailable":
				status = "rate_limited"
			case "bad-data":
				body = []byte(`null`)
			case "future":
				at = t0.Add(time.Second)
			case "zero-timestamp":
				at = time.UnixMilli(0)
			}
			if kind != "missing" {
				cacheFile(t, path, status, at, body)
			}
			switch kind {
			case "bad-json":
				write(t, path, []byte("invalid"))
			case "symlink":
				target := path + ".target"
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				os.Remove(path)
				if err := os.Mkdir(path, 0755); err != nil {
					t.Fatal(err)
				}
			}
			res, _, err := collect(t, src, t0, nil)
			if err == nil || len(res.Samples) != 0 {
				t.Fatalf("unusable file must fail without a reading: %v %v", res, err)
			}
			reasons := map[string]string{
				"stale": core.ReasonOther, "missing": core.ReasonNotFound,
				"unavailable": core.ReasonUpstream, "bad-json": core.ReasonParse,
				"bad-data": core.ReasonParse, "future": core.ReasonOther,
				"zero-timestamp": core.ReasonParse, "symlink": core.ReasonPermission,
				"directory": core.ReasonPermission,
			}
			wantReason := reasons[kind]
			if os.Geteuid() != 0 && kind != "missing" && kind != "symlink" && kind != "directory" {
				wantReason = "untrusted_owner"
			}
			var se *core.SourceError
			if !errorsAs(err, &se) || se.Reason != wantReason {
				t.Fatalf("reason: %v, want %s", err, wantReason)
			}
			if kind == "missing" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("missing file was created")
				}
			}
		})
	}
}

// A fresh local reading prevents even the first vendor request. An unavailable
// file permits the existing OAuth gates, and no OAuth opt-in means no request.
func TestStatuslineCacheFallbackThroughScheduler(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, fresh := range []bool{false, true} {
			t.Run(fmt.Sprintf("oauth=%t/fresh=%t", enabled, fresh), func(t *testing.T) {
				p := newCacheProfile(t)
				body := fixture(t, "oauth-usage-response.json")
				vendor := newStub(t, body)
				path := filepath.Join(t.TempDir(), "cache.json")
				local := &StatuslineCacheSource{P: p, Path: path, Interval: time.Minute, StaleAfter: 45 * time.Minute}
				if fresh {
					cacheFile(t, path, "available", t0.Add(-time.Minute), body)
				}
				sources := []core.Source{local}
				oauth := vendor.source(p)
				oauth.Statusline = local
				if enabled {
					sources = append(sources, oauth)
				}
				clk := &stepClock{t: t0}
				state, err := core.OpenState(t.TempDir(), t0)
				if err != nil {
					t.Fatal(err)
				}
				defer state.Close()
				sched := core.NewScheduler(sources, state, clk, slog.New(slog.NewTextHandler(io.Discard, nil)), 45*time.Minute)
				sched.Tick(context.Background(), nil, false, true)
				calls := 0
				if enabled && !fresh {
					calls = 1
				}
				if fresh && enabled && state.Get("claude-code/root/claude-oauth-usage").LastAttempt != 0 {
					t.Fatal("fresh cache consumed the persisted OAuth gate")
				}
				if vendor.calls != calls {
					t.Fatalf("vendor calls %d, want %d", vendor.calls, calls)
				}
				var out bytes.Buffer
				exp := &core.Exposition{Host: "node-a", Sched: sched, Clock: clk, ProcessStart: t0}
				exp.Render(&out)
				if !fresh {
					wantUp := `agent_source_up{host="node-a",harness="claude-code",profile="root",source="claude-statusline-cache"} 0`
					if !strings.Contains(out.String(), wantUp) || !strings.Contains(out.String(), `source="claude-statusline-cache",reason="not_found"} 1`) {
						t.Fatalf("missing cache health: %s", out.String())
					}
					if state.Get("claude-code/root/claude-statusline-cache").LastSuccess != 0 {
						t.Fatal("missing cache advanced last success")
					}
				}
				n := strings.Count(out.String(), "\nagent_quota_observed_timestamp_seconds{")
				want := 0
				if enabled || fresh {
					want = 1
				}
				if n != want {
					t.Fatalf("observed series %d want %d: %s", n, want, out.String())
				}
				// Recovering the file after OAuth ran must replace, never duplicate, its series.
				cacheFile(t, path, "available", t0.Add(time.Minute), body)
				clk.add(2 * time.Minute)
				sched.Tick(context.Background(), nil, false, true)
				out.Reset()
				exp.Render(&out)
				if strings.Count(out.String(), "\nagent_quota_observed_timestamp_seconds{") != 1 || vendor.calls != calls {
					t.Fatalf("recovery duplicated series or called vendor: %s", out.String())
				}
				// A stopped status line goes absent, or falls back after the unchanged floor.
				clk.add(46 * time.Minute)
				sched.Tick(context.Background(), nil, false, true)
				out.Reset()
				exp.Render(&out)
				if !enabled && strings.Contains(out.String(), "\nagent_quota_observed_timestamp_seconds{") {
					t.Fatal("stale reading served")
				}
				if enabled && vendor.calls != calls+1 {
					t.Fatalf("stale cache did not fall back: %d", vendor.calls)
				}
			})
		}
	}
}

// Timestamped files use the same interval-derived freshness limit between polls.
func TestStatuslineCacheExpiresBetweenPolls(t *testing.T) {
	p := newCacheProfile(t)
	path := filepath.Join(t.TempDir(), "cache.json")
	cacheFile(t, path, "available", t0.Add(-44*time.Minute), fixture(t, "oauth-usage-response.json"))
	local := &StatuslineCacheSource{P: p, Path: path, Interval: time.Hour, StaleAfter: 45 * time.Minute}
	clk := &stepClock{t: t0}
	state, err := core.OpenState(t.TempDir(), t0)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	sched := core.NewScheduler([]core.Source{local}, state, clk, slog.New(slog.NewTextHandler(io.Discard, nil)), 45*time.Minute)
	sched.Tick(context.Background(), nil, false, true)
	exp := &core.Exposition{Host: "node-a", Sched: sched, Clock: clk, ProcessStart: t0}
	var out bytes.Buffer
	exp.Render(&out)
	if !strings.Contains(out.String(), "\nagent_quota_observed_timestamp_seconds{") {
		t.Fatal("fresh reading missing")
	}
	clk.add(2 * time.Minute)
	out.Reset()
	exp.Render(&out)
	if !strings.Contains(out.String(), "\nagent_quota_observed_timestamp_seconds{") {
		t.Fatal("reading expired before the unified interval-derived limit")
	}
	clk.add(75*time.Minute + 26*time.Second)
	out.Reset()
	exp.Render(&out)
	if strings.Contains(out.String(), "\nagent_quota_observed_timestamp_seconds{") {
		t.Fatal("reading served beyond unified limit")
	}
}

// Fallback checks run every scheduler tick; they must share the file poll with
// Collect rather than reopening it every five seconds while OAuth is deferred.
func TestStatuslineCachePollCadence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	local := &StatuslineCacheSource{P: newCacheProfile(t), Path: path, Interval: time.Minute, StaleAfter: 45 * time.Minute}
	oauth := &OAuthUsageSource{Statusline: local}
	// Cache a missing-file result too: a new file is picked up at the next poll.
	if oauth.PreferLocal(t0) {
		t.Fatal("missing file preferred")
	}
	cacheFile(t, path, "available", t0, fixture(t, "oauth-usage-response.json"))
	if oauth.PreferLocal(t0.Add(5 * time.Second)) {
		t.Fatal("file reread before usage_interval")
	}
	if !oauth.PreferLocal(t0.Add(time.Minute)) {
		t.Fatal("new file missed at next poll")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	res, _, err := collect(t, local, t0.Add(65*time.Second), nil)
	if err != nil || len(res.Samples) == 0 {
		t.Fatal("Collect did not share fallback's cached file read")
	}
	if oauth.PreferLocal(t0.Add(2 * time.Minute)) {
		t.Fatal("removed file not detected at next poll")
	}
}

func TestStoppedStatuslineAllowsLiveQuota(t *testing.T) {
	p := newCacheProfile(t)
	body := fixture(t, "oauth-usage-response.json")
	vendor := newStub(t, body)
	path := filepath.Join(t.TempDir(), "cache.json")
	cacheFile(t, path, "available", t0, body)
	local := &StatuslineCacheSource{P: p, Path: path, Interval: time.Minute, StaleAfter: 45 * time.Minute}
	oauth := vendor.source(p)
	oauth.Statusline = local
	clk := &stepClock{t: t0}
	state, err := core.OpenState(t.TempDir(), t0)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	sched := core.NewScheduler([]core.Source{local, oauth}, state, clk, slog.New(slog.NewTextHandler(io.Discard, nil)), 45*time.Minute)
	sched.Tick(context.Background(), nil, false, true)
	if vendor.calls != 0 {
		t.Fatal("recent statusline did not defer OAuth")
	}
	clk.add(5*time.Minute + time.Second)
	sched.Tick(context.Background(), nil, false, true)
	if vendor.calls != 1 {
		t.Errorf("stopped statusline blocked OAuth: calls=%d", vendor.calls)
	}
	var out bytes.Buffer
	(&core.Exposition{Host: "node-a", Sched: sched, Clock: clk, ProcessStart: t0}).Render(&out)
	want := fmt.Sprintf("} %d", clk.Now().Unix())
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "agent_quota_observed_timestamp_seconds{") && !strings.HasSuffix(line, want) {
			t.Errorf("older local reading hid live quota: %s", line)
		}
	}
}

func TestStatuslineCacheTrust(t *testing.T) {
	for _, kind := range []string{"owner", "group-writable", "world-writable"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "owner" && os.Geteuid() != 0 {
				t.Skip("changing the cache owner needs root")
			}
			path := filepath.Join(t.TempDir(), "cache.json")
			// Invalid JSON distinguishes trust rejection before parsing.
			write(t, path, []byte("invalid"))
			switch kind {
			case "owner":
				if err := os.Chown(path, 65534, 65534); err != nil {
					t.Fatal(err)
				}
			case "group-writable":
				if err := os.Chmod(path, 0664); err != nil {
					t.Fatal(err)
				}
			case "world-writable":
				if err := os.Chmod(path, 0666); err != nil {
					t.Fatal(err)
				}
			}
			src := &StatuslineCacheSource{P: newProfile(t, "root"), Path: path, Interval: time.Minute, StaleAfter: 45 * time.Minute}
			res, _, err := collect(t, src, t0, nil)
			var se *core.SourceError
			if !errorsAs(err, &se) || se.Reason != "untrusted_owner" || len(res.Samples) != 0 {
				t.Fatalf("untrusted cache: %v %v", res, err)
			}
			if (&OAuthUsageSource{Statusline: src}).PreferLocal(t0) {
				t.Fatal("untrusted cache suppressed fallback")
			}
		})
	}
}

func TestStatuslineCacheCredentialsMtime(t *testing.T) {
	for _, offset := range []time.Duration{-time.Millisecond, 0, time.Millisecond} {
		t.Run(offset.String(), func(t *testing.T) {
			p := newCacheProfile(t)
			path := filepath.Join(t.TempDir(), "cache.json")
			cacheFile(t, path, "available", t0, fixture(t, "oauth-usage-response.json"))
			cred := filepath.Join(p.Dir, ".credentials.json")
			// Deliberately unparseable: this source must only stat credentials.
			write(t, cred, []byte("invalid"))
			mod := t0.Add(offset)
			if err := os.Chtimes(cred, mod, mod); err != nil {
				t.Fatal(err)
			}
			src := &StatuslineCacheSource{P: p, Path: path, Interval: time.Minute, StaleAfter: 45 * time.Minute}
			res, _, err := collect(t, src, t0, nil)
			want := offset <= 0
			if (err == nil) != want || (len(res.Samples) > 0) != want {
				t.Fatalf("credential mtime %v: samples=%d err=%v", mod, len(res.Samples), err)
			}
			if (&OAuthUsageSource{Statusline: src}).PreferLocal(t0) != want {
				t.Fatal("fallback disagrees with credential mtime")
			}
		})
	}
}

func TestStatuslineCacheReloginThroughScheduler(t *testing.T) {
	p := newCacheProfile(t)
	path := filepath.Join(t.TempDir(), "cache.json")
	body := fixture(t, "oauth-usage-response.json")
	cacheFile(t, path, "available", t0, body)
	local := &StatuslineCacheSource{P: p, Path: path, Interval: time.Minute, StaleAfter: 45 * time.Minute}
	vendor := newStub(t, body)
	oauth := vendor.source(p)
	oauth.Statusline = local
	clk := &stepClock{t: t0}
	state, err := core.OpenState(t.TempDir(), t0)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	sched := core.NewScheduler([]core.Source{local, oauth}, state, clk, slog.New(slog.NewTextHandler(io.Discard, nil)), 45*time.Minute)
	sched.Tick(context.Background(), nil, false, true)
	if vendor.calls != 0 {
		t.Fatal("fresh cache did not suppress OAuth")
	}
	cred := filepath.Join(p.Dir, ".credentials.json")
	mod := t0.Add(time.Second)
	if err := os.Chtimes(cred, mod, mod); err != nil {
		t.Fatal(err)
	}
	// Even between file polls, a changed login invalidates the cached reading.
	if oauth.PreferLocal(t0.Add(5 * time.Second)) {
		t.Fatal("pre-login cache preferred between polls")
	}
	clk.add(2 * time.Minute)
	sched.Tick(context.Background(), nil, false, true)
	if vendor.calls != 1 {
		t.Fatalf("fallback calls=%d", vendor.calls)
	}
	exp := &core.Exposition{Host: "node-a", Sched: sched, Clock: clk, ProcessStart: t0}
	var out bytes.Buffer
	exp.Render(&out)
	if !strings.Contains(out.String(), `source="claude-statusline-cache"} 0`) || !strings.Contains(out.String(), `source="claude-statusline-cache",reason="auth_expired"} 1`) {
		t.Fatalf("rejected cache still healthy: %s", out.String())
	}
	if strings.Count(out.String(), "\nagent_quota_observed_timestamp_seconds{") != 1 {
		t.Fatalf("OAuth reading hidden or duplicated: %s", out.String())
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "agent_quota_observed_timestamp_seconds{") && !strings.HasSuffix(line, fmt.Sprintf(" %d", clk.t.Unix())) {
			t.Fatalf("old cache hid OAuth: %s", line)
		}
	}
}
