package app

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/claude"
	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
)

var update = flag.Bool("update", false, "rewrite testdata/golden/metrics.txt")

var t0 = time.Unix(1767225600, 0).UTC() // fixture epoch, 2026-01-01T00:00:00Z

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func fx(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "claude", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func put(t *testing.T, path string, b []byte, appendTo bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendTo {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(b)
	f.Close()
}

type world struct {
	root  string
	homes string
	logs  bytes.Buffer
	calls int
}

// setup builds two Claude profiles on one machine: "acct-a" (quota opt-in) and "acct-b" (a different account,
// no quota), with the fixtures in place.
func setup(t *testing.T) (*world, *config.Config, *httptest.Server) {
	w := &world{root: t.TempDir()}
	w.homes = filepath.Join(w.root, "home")
	for _, h := range []string{"acct-a", "acct-b"} {
		dir := filepath.Join(w.homes, h, ".claude")
		cj := fx(t, "dot-claude.json")
		if h == "acct-b" {
			cj = bytes.ReplaceAll(cj, []byte("0000000000a1"), []byte("0000000000a2"))
		}
		put(t, filepath.Join(w.homes, h, ".claude.json"), cj, false)
		put(t, filepath.Join(dir, ".credentials.json"), fx(t, "dot-credentials.json"), false)
		put(t, filepath.Join(dir, "sessions", "4242.json"), fx(t, "sessions/4242.json"), false)
		put(t, filepath.Join(dir, "projects", "-root", "s.jsonl"), nil, false)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.calls++
		rw.Write(fx(t, "oauth-usage-response.json"))
	}))
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Host = "node-a"
	cfg.Labels = map[string]string{"env": "lab"}
	cfg.StateDir = filepath.Join(w.root, "state")
	cfg.Discovery.Roots = []string{filepath.Join(w.homes, "*")}
	cfg.Profiles = []config.Profile{{Harness: "claude-code", Alias: "work", Dir: filepath.Join(w.homes, "acct-a", ".claude"),
		Quota: config.Quota{"claude_oauth_usage": true}}}
	return w, cfg, srv
}

func wire(t *testing.T, w *world, cfg *config.Config, srv *httptest.Server, clk core.Clock) (*core.Scheduler, *core.Exposition, func()) {
	profiles, _ := cfg.Resolve(func(string) string { return "" })
	b := Build(cfg, profiles, cfg.Host, "test")
	for _, s := range b.Sources {
		if q, ok := s.(*claude.OAuthUsageSource); ok {
			q.Endpoint = srv.URL
			q.Transport = http.DefaultTransport
		}
	}
	st, err := core.OpenState(cfg.StateDir, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(&w.logs, nil))
	sched := core.NewScheduler(b.Sources, st, clk, log, cfg.Defaults.StaleAfter.D())
	exp := &core.Exposition{Host: cfg.Host, Static: b.Static, Version: "test", ProcessStart: t0, Profiles: b.Profiles, Sched: sched, Clock: clk}
	return sched, exp, st.Close
}

func renderString(t *testing.T, e *core.Exposition) string {
	var b bytes.Buffer
	if err := e.Render(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestEndToEnd is the acceptance run: two profiles, first start (no replay), new transcript lines, quota for the
// opted-in profile only, then the byte-exact /metrics text, contract conformance, no canary anywhere and no
// write under any profile directory.
func TestEndToEnd(t *testing.T) {
	defer func(f func(int) bool) { claude.ProcAlive = f }(claude.ProcAlive)
	claude.ProcAlive = func(pid int) bool { return pid == 4242 }
	w, cfg, srv := setup(t)
	clk := &clock{t: t0.Add(time.Minute)}
	sched, exp, closeState := wire(t, w, cfg, srv, clk)
	ctx := context.Background()

	sched.Tick(ctx, IsAccount, true, true)
	sched.Tick(ctx, nil, true, true) // first start: transcripts start at EOF
	for _, h := range []string{"acct-a", "acct-b"} {
		put(t, filepath.Join(w.homes, h, ".claude", "projects", "-root", "s.jsonl"), fx(t, "transcript-sample.jsonl"), true)
	}
	clk.add(2 * time.Minute)

	before := snapshot(t, w.homes)
	readOnly(t, w.homes)
	sched.Tick(ctx, nil, true, true)
	writable(t, w.homes)
	if after := snapshot(t, w.homes); after != before {
		t.Fatalf("profile tree changed during collection:\n%s\n---\n%s", before, after)
	}

	out := renderString(t, exp)
	golden := filepath.Join("..", "..", "testdata", "golden", "metrics.txt")
	if *update {
		os.MkdirAll(filepath.Dir(golden), 0o755)
		os.WriteFile(golden, []byte(out), 0o644)
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Fatalf("/metrics differs from golden (run go test ./internal/app -update after checking the diff):\n%s", out)
	}
	if w.calls != 1 {
		t.Fatalf("want exactly one vendor call (one opted-in account), got %d", w.calls)
	}
	if strings.Contains(out+w.logs.String(), "CANARY") {
		t.Fatal("canary in /metrics or logs")
	}
	checkContract(t, out)

	// Restart: counters continue, quota is served from the persisted answer without a new vendor call.
	closeState()
	clk.add(time.Minute)
	sched2, exp2, close2 := wire(t, w, cfg, srv, clk)
	defer close2()
	sched2.Tick(ctx, nil, true, true)
	out2 := renderString(t, exp2)
	for _, line := range []string{
		`agent_tokens_total{host="node-a",harness="claude-code",profile="work",account="acct-8bf0ba5b",provider="anthropic",model="model-x",type="output",env="lab"} 507`,
		`agent_quota_utilization_ratio{host="node-a",harness="claude-code",profile="work",account="acct-8bf0ba5b",provider="anthropic",window="5h",scope="all",env="lab"} 0.42`,
	} {
		if !strings.Contains(out2, line) {
			t.Fatalf("after restart missing %s\n%s", line, out2)
		}
	}
	if w.calls != 1 {
		t.Fatalf("restart called the vendor again inside the interval (%d calls)", w.calls)
	}
}

func snapshot(t *testing.T, root string) string {
	var b strings.Builder
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, _ := d.Info()
		rel, _ := filepath.Rel(root, p)
		fmt.Fprintf(&b, "%s %v %d %d\n", rel, fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
		return nil
	})
	return b.String()
}

// readOnly removes write permission from the whole profile tree, so any write attempt fails when the tests run
// as a non-root user (CI); the snapshot comparison covers root runs.
func readOnly(t *testing.T, root string) { chmodTree(t, root, 0o555, 0o444) }
func writable(t *testing.T, root string) { chmodTree(t, root, 0o755, 0o644) }

func chmodTree(t *testing.T, root string, dirMode, fileMode os.FileMode) {
	var paths []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		paths = append(paths, p)
		return nil
	})
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))
	if dirMode&0o200 != 0 {
		sort.Strings(paths)
	}
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			continue
		}
		m := fileMode
		if fi.IsDir() {
			m = dirMode
		}
		os.Chmod(p, m)
	}
	t.Cleanup(func() { writable2(root) })
}

func writable2(root string) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if d != nil && d.IsDir() {
			os.Chmod(p, 0o755)
		}
		return nil
	})
}

// contract parses specs/metric-contract.md tables into family → label set.
func contract(t *testing.T) map[string][]string {
	f, err := os.Open(filepath.Join("..", "..", "specs", "metric-contract.md"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	row := regexp.MustCompile("^\\| `([a-z_]+)` \\| [a-z ()]+ \\| [^|]+ \\| ([^|]+) \\|")
	tick := regexp.MustCompile("`([^`]+)`")
	out := map[string][]string{}
	var prev []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m := row.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		cell := strings.TrimSpace(m[2])
		var labels []string
		if strings.HasPrefix(cell, "same") {
			labels = append(labels, prev...)
		}
		for _, g := range tick.FindAllStringSubmatch(cell, -1) {
			labels = append(labels, strings.Fields(g[1])...)
		}
		out[m[1]] = labels
		prev = labels
	}
	if len(out) < 15 {
		t.Fatalf("contract parse found only %d metrics; table format changed?", len(out))
	}
	return out
}

// proseOnly: metrics the contract names in prose ("Counter semantics") but not in a table. Flagged to the
// maintainer as a spec gap; allowed here with no labels.
var proseOnly = map[string]bool{"agent_quota_exporter_state_epoch": true, "process_start_time_seconds": true}

func checkContract(t *testing.T, out string) {
	c := contract(t)
	series := regexp.MustCompile(`^([a-z_]+)(?:\{(.*)\})? `)
	name := regexp.MustCompile(`([a-z_]+)="`)
	for _, ln := range strings.Split(out, "\n") {
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		m := series.FindStringSubmatch(ln)
		if m == nil {
			t.Fatalf("unparsable line %q", ln)
		}
		var got []string
		for _, g := range name.FindAllStringSubmatch(m[2], -1) {
			if g[1] != "env" { // static config label
				got = append(got, g[1])
			}
		}
		want, ok := c[m[1]]
		if !ok {
			if proseOnly[m[1]] && len(got) == 0 {
				continue
			}
			t.Fatalf("metric %s is not in the contract", m[1])
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("%s labels %v, contract says %v", m[1], got, want)
		}
	}
	_ = io.EOF
}

// The contract parser must read label sets exactly, including "same" and "same + x" rows; otherwise the
// conformance check above would pass vacuously.
func TestContractParser(t *testing.T) {
	c := contract(t)
	for name, want := range map[string]string{
		"agent_sessions":                      "host harness profile account state",
		"agent_quota_limit_info":              "host harness profile account provider window scope severity",
		"agent_quota_reset_timestamp_seconds": "host harness profile account provider window scope",
		"agent_source_errors_total":           "host harness profile source reason",
		"agent_quota_exporter_build_info":     "version contract",
	} {
		if got := strings.Join(c[name], " "); got != want {
			t.Fatalf("%s: parsed %q, want %q", name, got, want)
		}
	}
	for _, f := range core.FamilyNames() {
		if _, ok := c[f]; !ok && !proseOnly[f] {
			t.Fatalf("renderer family %s is not in the contract", f)
		}
	}
}
