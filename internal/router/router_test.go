package router

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/sqlro"
)

// t0 is the time the fixture's health fields are written against (fixtures/router/rows.sql).
var t0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

const (
	c1 = "00000000-0000-4000-8000-000000000001"
	c2 = "00000000-0000-4000-8000-000000000002"
	c6 = "00000000-0000-4000-8000-000000000006"
)

// writer opens a read-write connection to <dir>/db/data.sqlite the way 9router would, schema applied.
func writer(t testing.TB, dir string, wal bool) (string, *sql.DB) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "db", "data.sqlite")
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)"
	if wal {
		dsn += "&_pragma=journal_mode(WAL)"
	}
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	w.SetMaxOpenConns(1)
	execAll(t, w, "router.schema.sql")
	return path, w
}

// execAll runs SQL statements; a name ending in .sql is read from fixtures/router.
func execAll(t testing.TB, w *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if strings.HasSuffix(s, ".sql") {
			b, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "router", s))
			if err != nil {
				t.Fatal(err)
			}
			s = string(b)
		}
		if _, err := w.Exec(s); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
}

func newSource(dir string, mut func(*config.Config)) *Source {
	cfg := config.Default()
	if mut != nil {
		mut(cfg)
	}
	srcs := Build(cfg, config.Resolved{Harness: config.HarnessRouter, Alias: "rt", Dir: dir, Origin: "config"}, "node-a")
	return srcs[0].(*Source)
}

// run collects once and returns the result and the state to pass next time (the old one if none was returned).
func run(t testing.TB, s *Source, now time.Time, st json.RawMessage) (core.Result, json.RawMessage) {
	t.Helper()
	res, next, err := s.Collect(context.Background(), now, st)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if next == nil {
		next = st
	}
	return res, next
}

// series renders samples as "name{k=v,...}" → value, labels sorted, host/harness/profile checked and dropped.
func series(t testing.TB, ss []core.Sample) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, s := range ss {
		var ls []string
		for _, l := range s.Labels {
			switch l.Name {
			case "host", "harness", "profile":
				if want := map[string]string{"host": "node-a", "harness": "9router", "profile": "rt"}[l.Name]; l.Value != want {
					t.Fatalf("%s: %s=%q, want %q", s.Name, l.Name, l.Value, want)
				}
				continue
			}
			ls = append(ls, l.Name+"="+l.Value)
		}
		sort.Strings(ls)
		k := s.Name + "{" + strings.Join(ls, ",") + "}"
		if _, dup := out[k]; dup {
			t.Fatalf("duplicate series %s", k)
		}
		out[k] = s.Value
	}
	return out
}

func hash(id string) string {
	h := sha256.Sum256([]byte(id))
	return "acct-" + hex.EncodeToString(h[:])[:8]
}

// usage returns the expected counter series of one (account, provider, model) total.
func usage(want map[string]float64, acct, prov, model string, in, out, ok, errs, cost float64) {
	b := "account=" + acct + ",model=" + model
	want["agent_tokens_total{"+b+",provider="+prov+",type=input}"] = in
	want["agent_tokens_total{"+b+",provider="+prov+",type=output}"] = out
	want["agent_requests_total{"+b+",outcome=ok,provider="+prov+"}"] = ok
	want["agent_requests_total{"+b+",outcome=error,provider="+prov+"}"] = errs
	if cost > 0 {
		want["agent_cost_usd_total{"+b+",provider="+prov+"}"] = cost
	}
}

func up(want map[string]float64, acct, prov string, v float64) {
	want["agent_router_connection_up{account="+acct+",provider="+prov+"}"] = v
}

func until(want map[string]float64, acct, prov string, v float64) {
	want["agent_router_rate_limited_until_timestamp_seconds{account="+acct+",provider="+prov+"}"] = v
}

// goldenHealth is the fixture's connection health at t0 in alias mode.
func goldenHealth() map[string]float64 {
	w := map[string]float64{}
	up(w, "work-a", "claude", 1)
	up(w, hash(c2), "codex", 0) // rate-limited until 13:00
	until(w, hash(c2), "codex", 1793797200)
	up(w, "or-key", "openrouter", 0) // last test failed
	up(w, "glm-1", "glm", 0)         // inactive; its rate limit is over, so no until series
	up(w, "gem-1", "gemini", 0)      // rate-limited until 14:00 (epoch ms)
	until(w, "gem-1", "gemini", 1793800800)
	up(w, hash(c6), "claude", 1) // duplicate name: hash
	up(w, "kiro-1", "kiro", 1)   // data not JSON: no failure recorded
	up(w, "qw-1", "qwen", 0)     // unknown testStatus word
	return w
}

func equalSeries(t *testing.T, got, want map[string]float64) {
	t.Helper()
	for k, v := range want {
		g, ok := got[k]
		if !ok {
			t.Errorf("missing %s", k)
		} else if math.Abs(g-v) > 1e-9 {
			t.Errorf("%s = %v, want %v", k, g, v)
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected %s = %v", k, v)
		}
	}
}

// Golden: every fixture row counted under the rules, health per connection, parse error for the unknown word.
func TestGolden(t *testing.T) {
	dir := t.TempDir()
	_, w := writer(t, dir, false)
	defer w.Close()
	s := newSource(dir, nil)
	res, st := run(t, s, t0, nil) // first start on an empty table: cursor 0
	if len(res.Samples) != 0 {
		t.Fatalf("empty database must give no series, got %v", res.Samples)
	}
	execAll(t, w, "rows.sql")
	res, _ = run(t, s, t0, st)

	want := goldenHealth()
	usage(want, "work-a", "claude", "claude-opus-4", 300, 30, 2, 0, 0.5)
	usage(want, hash(c2), "codex", "gpt-5", 700, 70, 2, 0, 0) // cost 0 everywhere: no cost series
	usage(want, "or-key", "openrouter", "deepseek/deepseek-chat", 110, 11, 1, 1, 0.03)
	usage(want, "glm-1", "other", "m1", 1, 1, 1, 0, 0)
	usage(want, "unknown", "claude", "claude-sonnet-4", 10, 1, 1, 0, 0)
	usage(want, "other", "claude", "claude-sonnet-4", 20, 2, 1, 0, 0)
	usage(want, hash(c6), "claude", "claude-opus-4", 5, 5, 0, 1, 0.1)
	equalSeries(t, series(t, res.Samples), want)
	if !reflect.DeepEqual(res.Errors, map[string]int{core.ReasonParse: 1}) {
		t.Errorf("errors = %v, want one parse error for the unknown testStatus", res.Errors)
	}
}

// No secret column value, email or connection id reaches a sample, an error or the persisted state.
func TestNoSecretsLeave(t *testing.T) {
	for _, mode := range []string{"alias", "email"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			_, w := writer(t, dir, false)
			defer w.Close()
			s := newSource(dir, func(c *config.Config) { c.Metrics.AccountLabel = mode })
			_, st := run(t, s, t0, nil)
			execAll(t, w, "rows.sql")
			res, st := run(t, s, t0, st)
			var all []string
			for _, x := range res.Samples {
				for _, l := range x.Labels {
					all = append(all, l.Value)
				}
			}
			all = append(all, string(st), fmt.Sprint(res.Errors))
			text := strings.Join(all, "\n")
			if strings.Contains(text, "CANARY") {
				t.Fatal("a secret field reached the output or the state")
			}
			if strings.Contains(text, "00000000-0000-4000") {
				t.Fatal("a raw connection id reached the output or the state")
			}
			if mode == "alias" && strings.Contains(text, "@") {
				t.Fatal("an email reached the output in alias mode")
			}
		})
	}
}

// The allow-list keeps every secret column out: not listed, and refused by sqlro before any SQL runs.
func TestAllowRefusesSecrets(t *testing.T) {
	secretCols := map[string]bool{"apiKey": true, "data": true, "tokens": true, "meta": true}
	for table, exprs := range allow {
		for _, e := range exprs {
			switch v := e.(type) {
			case sqlro.Col:
				if secretCols[string(v)] {
					t.Errorf("%s.%s is listed as a bare column", table, v)
				}
			case sqlro.JSON:
				if table != "providerConnections" || v.Col != "data" || (v.Path != "$.testStatus" && v.Path != "$.rateLimitedUntil") {
					t.Errorf("unexpected JSON extract %s.%s %s", table, v.Col, v.Path)
				}
			}
		}
	}

	dir := t.TempDir()
	path, w := writer(t, dir, false)
	execAll(t, w, "rows.sql")
	w.Close()
	db, err := sqlro.Open(path, allow)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	refused := []struct {
		table string
		e     sqlro.Expr
	}{
		{"usageHistory", sqlro.Col("apiKey")},
		{"usageHistory", sqlro.Col("tokens")},
		{"usageHistory", sqlro.Col("meta")},
		{"usageHistory", sqlro.JSON{Col: "tokens", Path: "$.prompt_tokens"}},
		{"providerConnections", sqlro.Col("data")},
		{"providerConnections", sqlro.JSON{Col: "data", Path: "$.accessToken"}},
		{"providerConnections", sqlro.JSON{Col: "data", Path: "$.refreshToken"}},
		{"providerConnections", sqlro.JSON{Col: "data", Path: "$.idToken"}},
		{"providerConnections", sqlro.JSON{Col: "data", Path: "$.apiKey"}},
	}
	for _, r := range refused {
		rows, err := db.After(r.table, "rowid", 0, []sqlro.Expr{r.e}, 10)
		if err == nil || rows != nil {
			t.Errorf("%s %v: must be refused", r.table, r.e)
		}
	}
	// the listed reads work on the same database
	if _, err := db.After("usageHistory", "id", 0, usageCols, 10); err != nil {
		t.Fatalf("listed usage read refused: %v", err)
	}
}

// First start counts nothing already in the table; only rows written afterwards.
func TestNoReplayAtFirstStart(t *testing.T) {
	dir := t.TempDir()
	_, w := writer(t, dir, false)
	defer w.Close()
	execAll(t, w, "rows.sql")
	s := newSource(dir, nil)
	res, st := run(t, s, t0, nil)
	got := series(t, res.Samples)
	equalSeries(t, got, goldenHealth()) // health is a gauge: served at once; no counter yet
	execAll(t, w, `INSERT INTO usageHistory (id, provider, model, connectionId, apiKey, promptTokens, completionTokens, cost, status)
		VALUES (11, 'claude', 'claude-opus-4', '`+c1+`', 'CANARY', 7, 3, 0, 'ok')`)
	res, _ = run(t, s, t0, st)
	want := goldenHealth()
	usage(want, "work-a", "claude", "claude-opus-4", 7, 3, 1, 0, 0)
	equalSeries(t, series(t, res.Samples), want)
}

func insertUsage(t testing.TB, w *sql.DB, from, n int, conn, model string) {
	t.Helper()
	tx, err := w.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO usageHistory (id, provider, model, connectionId, apiKey, promptTokens, completionTokens, cost, status, tokens, meta)
		VALUES (?, 'claude', ?, ?, 'CANARY-KEY', 2, 1, 0, 'ok', '{"prompt_tokens":2}', '{"x":"CANARY-META"}')`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := stmt.Exec(from+i, model, conn); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func requests(t testing.TB, res core.Result, acct, model string) float64 {
	return series(t, res.Samples)["agent_requests_total{account="+acct+",model="+model+",outcome=ok,provider=claude}"]
}

// A backlog is read batch rows per cycle, in order, across cycles and across a restart, each row once.
func TestBatchesAcrossCyclesAndRestart(t *testing.T) {
	dir := t.TempDir()
	_, w := writer(t, dir, false)
	defer w.Close()
	execAll(t, w, "rows.sql")
	s := newSource(dir, nil)
	_, st := run(t, s, t0, nil) // cursor at 10
	insertUsage(t, w, 11, 2*batch+1234, c1, "m")
	for i, want := range []float64{batch, 2 * batch} {
		var res core.Result
		res, st = run(t, s, t0, st)
		if got := requests(t, res, "work-a", "m"); got != want {
			t.Fatalf("cycle %d: requests = %v, want %v", i+1, got, want)
		}
	}
	// restart: a new instance continues from the persisted cursor and totals
	s2 := newSource(dir, nil)
	res, st := run(t, s2, t0, st)
	if got := requests(t, res, "work-a", "m"); got != 2*batch+1234 {
		t.Fatalf("after restart: requests = %v, want %v", got, 2*batch+1234)
	}
	if got := series(t, res.Samples)["agent_tokens_total{account=work-a,model=m,provider=claude,type=input}"]; got != 2*(2*batch+1234) {
		t.Fatalf("after restart: input tokens = %v", got)
	}
	res, _ = run(t, s2, t0, st)
	if got := requests(t, res, "work-a", "m"); got != 2*batch+1234 {
		t.Fatalf("idle cycle changed the count: %v", got)
	}
}

// The cursor never goes back to re-count: a table whose ids shrank restarts counting from its new top.
func TestCursorShrinkNoRecount(t *testing.T) {
	dir := t.TempDir()
	_, w := writer(t, dir, false)
	defer w.Close()
	s := newSource(dir, nil)
	_, st := run(t, s, t0, nil)
	execAll(t, w, "rows.sql")
	_, st = run(t, s, t0, st) // counted 1..10
	execAll(t, w, `DELETE FROM usageHistory WHERE id > 5`)
	_, st = run(t, s, t0, st) // cursor to 5
	insertUsage(t, w, 6, 1, c1, "m")
	res, _ := run(t, s, t0, st)
	if got := requests(t, res, "work-a", "claude-opus-4"); got != 2 {
		t.Fatalf("old rows were re-counted: %v", got)
	}
	if got := requests(t, res, "work-a", "m"); got != 1 {
		t.Fatalf("a row after the shrunk top was not counted: %v", got)
	}
}

func TestConnectionLabels(t *testing.T) {
	if got := hashID(c1); got != hash(c1) || !strings.HasPrefix(got, "acct-") || len(got) != 13 {
		t.Fatalf("hashID = %q, want acct- + 8 hex of sha256", got)
	}
	s := &Source{EmailMode: true}
	cases := []struct {
		name, email string
		used        []string
		want        string
	}{
		{"Work-A", "", nil, "work-a"},
		{"bad name!", "x@example.com", nil, "x@example.com"},
		{"bad name!", "", nil, hash("id")},
		{"bad name!", "not-an-email", nil, hash("id")},
		{"other", "", []string{"other", "unknown"}, hash("id")}, // reserved words are never a connection label
		{"work-a", "x@example.com", []string{"work-a"}, "x@example.com"},
		{"work-a", "", []string{"work-a", hash("id")}, ""}, // every candidate taken: folded
		{strings.Repeat("a", 49), "", nil, hash("id")},
	}
	for _, c := range cases {
		used := map[string]bool{}
		for _, u := range c.used {
			used[u] = true
		}
		if got := s.connLabel("id", c.name, c.email, used); got != c.want {
			t.Errorf("connLabel(%q, %q, %v) = %q, want %q", c.name, c.email, c.used, got, c.want)
		}
	}
	// alias mode ignores an email even if one is passed
	if got := (&Source{}).connLabel("id", "bad name!", "x@example.com", map[string]bool{}); got != hash("id") {
		t.Errorf("alias mode used the email: %q", got)
	}

	// email mode end to end: the unsafe name and the duplicate name show their emails
	dir := t.TempDir()
	_, w := writer(t, dir, false)
	defer w.Close()
	execAll(t, w, "rows.sql")
	res, _ := run(t, newSource(dir, func(c *config.Config) { c.Metrics.AccountLabel = "email" }), t0, nil)
	got := series(t, res.Samples)
	for _, k := range []string{"agent_router_connection_up{account=b1@example.com,provider=codex}",
		"agent_router_connection_up{account=d1@example.com,provider=claude}",
		"agent_router_connection_up{account=work-a,provider=claude}"} {
		if _, ok := got[k]; !ok {
			t.Errorf("email mode: missing %s", k)
		}
	}
}

// Past maxConns connections, health is not emitted and usage folds into account="other"; a freed slot is reused.
func TestConnectionCap(t *testing.T) {
	dir := t.TempDir()
	_, w := writer(t, dir, false)
	defer w.Close()
	id := func(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }
	for i := 1; i <= maxConns+5; i++ {
		execAll(t, w, fmt.Sprintf(`INSERT INTO providerConnections (id, provider, name, isActive, data)
			VALUES ('%s', 'claude', 'conn-%d', 1, '{"testStatus":"active","accessToken":"CANARY"}')`, id(i), i))
	}
	s := newSource(dir, nil)
	_, st := run(t, s, t0, nil)
	insertUsage(t, w, 1, 3, id(1), "m")
	insertUsage(t, w, 4, 2, id(maxConns+2), "m")
	res, st := run(t, s, t0, st)
	got := series(t, res.Samples)
	n := 0
	for k := range got {
		if strings.HasPrefix(k, "agent_router_connection_up{") {
			n++
		}
	}
	if n != maxConns {
		t.Fatalf("%d health series, want %d", n, maxConns)
	}
	if _, ok := got[fmt.Sprintf("agent_router_connection_up{account=conn-%d,provider=claude}", maxConns+1)]; ok {
		t.Fatal("a connection past the cap has a health series")
	}
	if requests(t, res, "conn-1", "m") != 3 || requests(t, res, "other", "m") != 2 {
		t.Fatalf("usage not split as named/other: %v", got)
	}

	// deleting a named connection frees its slot for the first folded one; the deleted label's counters end
	execAll(t, w, `DELETE FROM providerConnections WHERE id = '`+id(1)+`'`)
	insertUsage(t, w, 6, 1, id(maxConns+1), "m")
	res, _ = run(t, s, t0, st)
	got = series(t, res.Samples)
	if _, ok := got[fmt.Sprintf("agent_router_connection_up{account=conn-%d,provider=claude}", maxConns+1)]; !ok {
		t.Fatal("the freed slot was not given to the next connection")
	}
	if _, ok := got["agent_router_connection_up{account=conn-1,provider=claude}"]; ok {
		t.Fatal("a deleted connection still has a health series")
	}
	if requests(t, res, "conn-1", "m") != 0 || requests(t, res, fmt.Sprintf("conn-%d", maxConns+1), "m") != 1 {
		t.Fatalf("labels after the slot moved: %v", got)
	}
}

// Models are capped per profile across connections; a model past the cap counts into model="other".
func TestModelCap(t *testing.T) {
	dir := t.TempDir()
	_, w := writer(t, dir, false)
	defer w.Close()
	execAll(t, w, "rows.sql")
	s := newSource(dir, func(c *config.Config) { c.Metrics.MaxModelsPerProfile = 2 })
	_, st := run(t, s, t0, nil)
	insertUsage(t, w, 11, 1, c1, "m-a")
	insertUsage(t, w, 12, 1, c6, "m-b") // another connection: same per-profile budget
	insertUsage(t, w, 13, 1, c1, "m-c")
	insertUsage(t, w, 14, 1, c6, "m-d")
	res, st := run(t, s, t0, st)
	got := series(t, res.Samples)
	if requests(t, res, "work-a", "m-a") != 1 || requests(t, res, hash(c6), "m-b") != 1 ||
		requests(t, res, "work-a", "other") != 1 || requests(t, res, hash(c6), "other") != 1 {
		t.Fatalf("model cap: %v", got)
	}
	for k := range got {
		if strings.Contains(k, "model=m-c") || strings.Contains(k, "model=m-d") {
			t.Fatalf("a model past the cap has its own series: %s", k)
		}
	}
	// a day later the oldest named model's slot is free for a new one
	insertUsage(t, w, 15, 1, c1, "m-c")
	res, _ = run(t, s, t0.Add(25*time.Hour), st)
	if requests(t, res, "work-a", "m-c") != 1 {
		t.Fatalf("stale model slot not reused: %v", series(t, res.Samples))
	}
}

// Health follows the clock: a rate limit that ended reads up again without a new read of the database.
func TestRateLimitEnds(t *testing.T) {
	dir := t.TempDir()
	_, w := writer(t, dir, false)
	defer w.Close()
	execAll(t, w, "rows.sql")
	s := newSource(dir, nil)
	_, st := run(t, s, t0, nil)
	res, _ := run(t, s, t0.Add(90*time.Minute), st) // 13:30: c2's limit (13:00) is over, gem-1's (14:00) is not
	got := series(t, res.Samples)
	if got["agent_router_connection_up{account="+hash(c2)+",provider=codex}"] != 1 {
		t.Error("c2 must be up after its rate limit ended")
	}
	if _, ok := got["agent_router_rate_limited_until_timestamp_seconds{account="+hash(c2)+",provider=codex}"]; ok {
		t.Error("an ended rate limit must not have an until series")
	}
	if got["agent_router_connection_up{account=gem-1,provider=gemini}"] != 0 ||
		got["agent_router_rate_limited_until_timestamp_seconds{account=gem-1,provider=gemini}"] != 1793800800 {
		t.Error("gem-1 is still rate-limited at 13:30")
	}
}

// An idle WAL database (no -wal/-shm) is read immutable by sqlro: counters and health are served, also after a
// restart, and no sidecar is created in the router's directory.
func TestIdleWALServesPersisted(t *testing.T) {
	dir := t.TempDir()
	path, w := writer(t, dir, true)
	s := newSource(dir, nil)
	_, st := run(t, s, t0, nil)
	execAll(t, w, "rows.sql")
	live, st := run(t, s, t0, st)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, sc := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(path + sc); err == nil {
			t.Fatalf("precondition: %s left after the writer closed", sc)
		}
	}
	idle, st2 := run(t, s, t0, st)
	restarted, _ := run(t, newSource(dir, nil), t0, st2)
	for _, sc := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(path + sc); err == nil {
			t.Fatalf("reading an idle WAL database created %s", sc)
		}
	}
	want := series(t, live.Samples)
	equalSeries(t, series(t, idle.Samples), want)
	equalSeries(t, series(t, restarted.Samples), want)
	if len(want) < 20 {
		t.Fatalf("the live read served too little to compare: %d series", len(want))
	}
}

func reasonOf(err error) string {
	var se *core.SourceError
	if errors.As(err, &se) {
		return se.Reason
	}
	return fmt.Sprintf("%T", err)
}

func TestMissingAndBusy(t *testing.T) {
	s := newSource(t.TempDir(), nil)
	if _, _, err := s.Collect(context.Background(), t0, nil); reasonOf(err) != core.ReasonNotFound {
		t.Fatalf("missing database: %v, want not_found", err)
	}

	dir := t.TempDir()
	_, w := writer(t, dir, false) // rollback journal: an exclusive lock blocks readers
	defer w.Close()
	execAll(t, w, "rows.sql")
	s = newSource(dir, nil)
	_, st := run(t, s, t0, nil)
	ctx := context.Background()
	lock, err := w.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	res, next, err := s.Collect(ctx, t0, st)
	lock.ExecContext(ctx, "ROLLBACK")
	lock.Close()
	if reasonOf(err) != core.ReasonTimeout || next != nil || len(res.Samples) != 0 {
		t.Fatalf("busy database: err=%v state=%v samples=%d, want timeout and nothing", err, next != nil, len(res.Samples))
	}
}

func TestValueMapping(t *testing.T) {
	for _, c := range []struct {
		v    any
		want int64
		ok   bool
	}{
		{nil, 0, true}, {"", 0, true}, {"2026-11-04T13:00:00.000Z", 1793797200, true},
		{"2026-11-04T20:00:00+07:00", 1793797200, true}, {int64(1793797200000), 1793797200, true},
		{int64(1793797200), 1793797200, true}, {float64(1793797200000), 1793797200, true}, {"1793797200", 1793797200, true},
		{int64(0), 0, true}, {int64(-5), 0, true}, {"soon", 0, false}, {float64(1e30), 0, false},
	} {
		got, ok := untilSeconds(c.v)
		if got != c.want || ok != c.ok {
			t.Errorf("untilSeconds(%v) = %d, %v; want %d, %v", c.v, got, ok, c.want, c.ok)
		}
	}
	for v, want := range map[any]bool{nil: true, "": true, "ok": true, "OK": true, "success": true, "200": true,
		int64(204): true, "error": false, "failed": false, "429": false, int64(500): false, "timeout": false} {
		if statusOK(v) != want {
			t.Errorf("statusOK(%v) != %v", v, want)
		}
	}
	for in, want := range map[string]string{"OpenRouter": "openrouter", "": "unknown", "weird provider!": "other",
		strings.Repeat("p", 257): "other", "cc": "cc", strings.Repeat("p", 48): strings.Repeat("p", 48)} {
		if got := providerLabel(in); got != want {
			t.Errorf("providerLabel(%q) = %q, want %q", in, got, want)
		}
	}

	// 9router's custom providers are "<kind>-compatible-<uuid>", over 48 characters. Two of them must not merge into
	// one series, and the label keeps the readable kind and never the uuid.
	a := providerLabel("anthropic-compatible-0f8e2c1a-3b4d-4e5f-8a9b-0c1d2e3f4a5b")
	b := providerLabel("Anthropic-Compatible-7a6b5c4d-3e2f-4a1b-9c8d-7e6f5a4b3c2d")
	c := providerLabel("openai-compatible-chat-0f8e2c1a-3b4d-4e5f-8a9b-0c1d2e3f4a5b")
	long := providerLabel(strings.Repeat("p", 49))
	for _, l := range []string{a, b, c, long} {
		if !labelRe.MatchString(l) {
			t.Errorf("label %q is not label-safe or over 48", l)
		}
	}
	if a == b || a == c || b == c {
		t.Errorf("custom providers merged: %q %q %q", a, b, c)
	}
	if !strings.HasPrefix(a, "anthropic-compatible-") || !strings.HasPrefix(c, "openai-compatible-chat-") ||
		strings.Contains(a+c, "0f8e2c1a") || !strings.HasPrefix(long, strings.Repeat("p", 39)+"-") {
		t.Errorf("prefix not kept or uuid leaked: %q %q %q", a, c, long)
	}
	if providerLabel("anthropic-compatible-0f8e2c1a-3b4d-4e5f-8a9b-0c1d2e3f4a5b") != a {
		t.Error("label not stable")
	}
}

// BenchmarkCycle reads a 200k-row usageHistory backlog batch rows per cycle and reports time and heap per cycle.
func BenchmarkCycle(b *testing.B) {
	dir := b.TempDir()
	_, w := writer(b, dir, false)
	execAll(b, w, "rows.sql")
	s := newSource(dir, nil)
	_, st, err := s.Collect(context.Background(), t0, nil)
	if err != nil {
		b.Fatal(err)
	}
	const rows = 200000
	insertUsage(b, w, 11, rows, c1, "m")
	w.Close()
	var ms runtime.MemStats
	b.ResetTimer()
	var peak uint64
	for i := 0; i < b.N; i++ {
		if i%(rows/batch) == 0 { // rewind so every cycle reads a full batch
			s.st.Cursor = 10
		}
		if _, next, err := s.Collect(context.Background(), t0, st); err != nil {
			b.Fatal(err)
		} else if next != nil {
			st = next
		}
		runtime.ReadMemStats(&ms)
		peak = max(peak, ms.HeapAlloc)
	}
	b.ReportMetric(float64(peak)/(1<<20), "peak-heap-MiB")
	b.ReportMetric(batch, "rows/cycle")
}

// A connection name is used as the account label only if, lowercased, it is label-safe and does not look like a
// credential; otherwise the label is the hash of the connection id, so a key pasted into a name never becomes
// a label value.
func TestConnLabelRefusesKeyLikeNames(t *testing.T) {
	s := &Source{}
	for name, wantName := range map[string]bool{
		"work-a":                      true,
		"sk-abc123":                   false,
		"SK-AbC123":                   false, // lowercased first, then refused
		"aizasyexampleexample":        false,
		"team-0123456789abcdefghijkl": false, // 22-char run
		"openai.main_2":               true,
	} {
		got := s.connLabel("00000000-0000-4000-8000-000000000042", name, "", map[string]bool{})
		if (got == name) != wantName {
			t.Errorf("name %q: label %q, want name used=%v", name, got, wantName)
		}
		if !wantName && !strings.HasPrefix(got, "acct-") {
			t.Errorf("name %q: want hash label, got %q", name, got)
		}
	}
}
