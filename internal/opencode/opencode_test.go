package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/sqlro"
)

// canary is planted in every content and secret field of the test database; it must never leave the plugin.
const canary = "CANARY-OPENCODE-7c41e2"

var t0 = time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)

// writer opens a read-write connection the way opencode would; wal leaves the database in WAL mode.
func writer(t *testing.T, path string, wal bool) *sql.DB {
	t.Helper()
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)"
	if wal {
		dsn += "&_pragma=journal_mode(WAL)"
	}
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	w.SetMaxOpenConns(1)
	if err := w.Ping(); err != nil {
		t.Fatal(err)
	}
	return w
}

func exec(t *testing.T, w *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if strings.HasSuffix(s, ".sql") {
			b, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "opencode", s))
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

// emptyDB is the fixture schema with no rows; the writer stays open (and is closed at cleanup).
func emptyDB(t *testing.T, wal bool) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), dbFile)
	w := writer(t, path, wal)
	t.Cleanup(func() { w.Close() })
	exec(t, w, "opencode.schema.sql")
	return path, w
}

// insert adds one message row to the fixture session.
func insert(t *testing.T, w *sql.DB, id, data string) {
	t.Helper()
	if _, err := w.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data)
		VALUES (?, 'ses_00000000000000000001', 1767225600000, 1767225600000, ?)`, id, data); err != nil {
		t.Fatal(err)
	}
}

func update(t *testing.T, w *sql.DB, id, data string) {
	t.Helper()
	if _, err := w.Exec(`UPDATE message SET data = ?, time_updated = time_updated + 1 WHERE id = ?`, data, id); err != nil {
		t.Fatal(err)
	}
}

// done is a complete assistant message.
func done(provider, model string, in, out, reasoning, cr, cw int, cost float64) string {
	return fmt.Sprintf(`{"role":"assistant","time":{"created":1767225600000,"completed":1767225601000},`+
		`"providerID":%q,"modelID":%q,"cost":%v,"tokens":{"total":0,"input":%d,"output":%d,"reasoning":%d,`+
		`"cache":{"read":%d,"write":%d}},"finish":"stop"}`, provider, model, cost, in, out, reasoning, cr, cw)
}

// streaming is an assistant message still in flight: no time.completed, partial tokens.
func streaming(provider, model string, out int) string {
	return fmt.Sprintf(`{"role":"assistant","time":{"created":1767225600000},"providerID":%q,"modelID":%q,`+
		`"cost":0,"tokens":{"total":0,"input":0,"output":%d,"reasoning":0,"cache":{"read":0,"write":0}}}`,
		provider, model, out)
}

func failed(name string, status int) string {
	return fmt.Sprintf(`{"role":"assistant","time":{"created":1767225600000,"completed":1767225601000},`+
		`"providerID":"provider-a","modelID":"model-y","cost":0,"tokens":{"input":0,"output":0},`+
		`"error":{"name":%q,"data":{"statusCode":%d,"message":%q}}}`, name, status, canary)
}

func source(path string, maxModels int) *DBSource {
	return &DBSource{Host: "node-a", Alias: "lane-1", Account: "acct-b1", Path: path, MaxModels: maxModels,
		Interval: time.Minute}
}

// series renders samples as `name{provider,model,extra}` → value; host, harness, profile and account are
// checked once in TestGolden.
func series(res core.Result) map[string]float64 {
	out := map[string]float64{}
	for _, s := range res.Samples {
		var ls []string
		for _, l := range s.Labels[4:] {
			ls = append(ls, l.Value)
		}
		k := s.Name + "{" + strings.Join(ls, ",") + "}"
		if _, dup := out[k]; dup {
			panic("duplicate series " + k)
		}
		out[k] = s.Value
	}
	return out
}

type run struct {
	res   core.Result
	state json.RawMessage
}

// collect runs one cycle and threads the persisted state the way the scheduler does.
func (r *run) collect(t *testing.T, s *DBSource, now time.Time) map[string]float64 {
	t.Helper()
	res, st, err := s.Collect(context.Background(), now, r.state)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if st != nil {
		r.state = st
	}
	r.res = res
	return series(res)
}

// tokens returns the five token series and the two request series of one (provider, model).
func counters(pm string, in, out, rs, cr, cw, ok, errs float64) map[string]float64 {
	return map[string]float64{
		"agent_tokens_total{" + pm + ",input}":       in,
		"agent_tokens_total{" + pm + ",output}":      out,
		"agent_tokens_total{" + pm + ",reasoning}":   rs,
		"agent_tokens_total{" + pm + ",cache_read}":  cr,
		"agent_tokens_total{" + pm + ",cache_write}": cw,
		"agent_requests_total{" + pm + ",ok}":        ok,
		"agent_requests_total{" + pm + ",error}":     errs,
	}
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

func eq(t *testing.T, what string, got, want map[string]float64) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		var keys []string
		for k := range got {
			keys = append(keys, k)
		}
		for k := range want {
			if _, ok := got[k]; !ok {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			if got[k] != want[k] {
				fmt.Fprintf(&b, "\n  %s: got %v want %v", k, got[k], want[k])
			}
		}
		t.Fatalf("%s:%s", what, b.String())
	}
}

// Every counter from the shared fixture plus a cached, mixed-case row, with the full label set.
func TestGolden(t *testing.T) {
	path, w := emptyDB(t, false)
	s, r := source(path, 20), &run{}
	if got := r.collect(t, s, t0); len(got) != 0 {
		t.Fatalf("first collect on an empty table: %v", got)
	}
	exec(t, w, "rows.sql")
	insert(t, w, "msg_00000000000000000005", done("Provider-B", "Model-Z", 10, 20, 5, 100, 7, 0))

	got := r.collect(t, s, t0.Add(time.Minute))
	want := merge(
		// msg 2 (ok, cost 0.0037) and msg 3 (APIError 429); the user row msg 1 counts nothing.
		counters("provider-a,model-y", 24545, 3, 16, 0, 0, 1, 1),
		map[string]float64{
			"agent_request_errors_total{provider-a,model-y,rate_limited}": 1,
			"agent_cost_usd_total{provider-a,model-y}":                    0.0037,
		},
		// provider and model are lowercased; cost 0 means no cost series.
		counters("provider-b,model-z", 10, 20, 5, 100, 7, 1, 0),
	)
	eq(t, "golden", got, want)
	if r.res.Errors != nil {
		t.Fatalf("errors: %v", r.res.Errors)
	}
	l := r.res.Samples[0].Labels
	if l[0] != (core.Label{Name: "host", Value: "node-a"}) || l[1] != (core.Label{Name: "harness", Value: "opencode"}) ||
		l[2] != (core.Label{Name: "profile", Value: "lane-1"}) || l[3] != (core.Label{Name: "account", Value: "acct-b1"}) ||
		l[4].Name != "provider" || l[5].Name != "model" {
		t.Fatalf("labels: %v", l)
	}
	// A cycle with nothing new changes nothing.
	eq(t, "idle cycle", r.collect(t, s, t0.Add(2*time.Minute)), want)
}

// Build wires the profile: account falls back to the alias, interval to the usage default, path to <dir>/opencode.db.
func TestBuild(t *testing.T) {
	cfg := config.Default()
	srcs := Build(cfg, config.Resolved{Harness: "opencode", Alias: "lane-2", Dir: "/data/oc"}, "node-a")
	s := srcs[0].(*DBSource)
	if len(srcs) != 1 || s.Name() != "opencode-db" || s.Harness() != "opencode" || s.Profile() != "lane-2" ||
		s.Account != "lane-2" || s.Path != "/data/oc/opencode.db" || s.Network() ||
		s.MinInterval() != cfg.Defaults.UsageInterval.D() || s.MaxModels != cfg.Metrics.MaxModelsPerProfile {
		t.Fatalf("build: %+v", s)
	}
	srcs = Build(cfg, config.Resolved{Harness: "opencode", Alias: "lane-2", Account: "acct-x", Dir: "/d"}, "h")
	if srcs[0].(*DBSource).Account != "acct-x" {
		t.Fatal("configured account must win over the alias")
	}
}

// opencode inserts an assistant row when the call starts and updates it as it streams and ends. The row counts
// once, when complete, with its final tokens; later updates of a counted row count nothing.
func TestCountedOnceWhenComplete(t *testing.T) {
	path, w := emptyDB(t, false)
	s, r := source(path, 20), &run{}
	r.collect(t, s, t0)

	insert(t, w, "msg_a", streaming("provider-a", "model-y", 5))
	if got := r.collect(t, s, t0.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("an incomplete message must not count: %v", got)
	}
	update(t, w, "msg_a", streaming("provider-a", "model-y", 50))
	if got := r.collect(t, s, t0.Add(2*time.Minute)); len(got) != 0 {
		t.Fatalf("still incomplete: %v", got)
	}
	update(t, w, "msg_a", done("provider-a", "model-y", 100, 200, 30, 40, 50, 0.01))
	want := merge(counters("provider-a,model-y", 100, 200, 30, 40, 50, 1, 0),
		map[string]float64{"agent_cost_usd_total{provider-a,model-y}": 0.01})
	eq(t, "final values", r.collect(t, s, t0.Add(3*time.Minute)), want)

	update(t, w, "msg_a", done("provider-a", "model-y", 999, 999, 999, 999, 999, 9))
	eq(t, "counted once", r.collect(t, s, t0.Add(4*time.Minute)), want)
	if len(s.st.Pending) != 0 {
		t.Fatalf("pending after completion: %v", s.st.Pending)
	}
}

// An errored call counts as outcome="error" with a bounded code from the error's name and status only.
func TestErrors(t *testing.T) {
	path, w := emptyDB(t, false)
	s, r := source(path, 20), &run{}
	r.collect(t, s, t0)
	insert(t, w, "e1", failed("APIError", 429))
	insert(t, w, "e2", failed("ProviderAuthError", 0))
	insert(t, w, "e3", failed("APIError", 529))
	insert(t, w, "e4", failed("APIError", 500))
	insert(t, w, "e5", failed("MessageAbortedError", 0))
	// An error ends the call even if time.completed was never written.
	insert(t, w, "e6", `{"role":"assistant","providerID":"provider-a","modelID":"model-y","error":{"name":"APIError","data":{"statusCode":401}}}`)
	got := r.collect(t, s, t0.Add(time.Minute))
	want := merge(counters("provider-a,model-y", 0, 0, 0, 0, 0, 0, 6), map[string]float64{
		"agent_request_errors_total{provider-a,model-y,rate_limited}": 1,
		"agent_request_errors_total{provider-a,model-y,auth}":         2,
		"agent_request_errors_total{provider-a,model-y,overloaded}":   1,
		"agent_request_errors_total{provider-a,model-y,other}":        2,
	})
	eq(t, "errors", got, want)
}

func TestErrCode(t *testing.T) {
	for _, c := range []struct {
		name   string
		status float64
		want   string
	}{
		{"APIError", 429, "rate_limited"}, {"APIError", 401, "auth"}, {"APIError", 403, "auth"},
		{"APIError", 503, "overloaded"}, {"APIError", 529, "overloaded"}, {"APIError", 500, "other"},
		{"APIError", 0, "other"}, {"ProviderAuthError", 0, "auth"}, {"UnknownError", 429, "other"},
		{"ContextOverflowError", 0, "other"}, {canary, 0, "other"},
	} {
		if got := errCode(c.name, c.status); got != c.want {
			t.Errorf("errCode(%q, %v) = %q, want %q", c.name, c.status, got, c.want)
		}
	}
}

// Cost is exported for a series only once it has reported a non-zero cost; until then it is absent, not 0.
func TestCostOnlyAfterNonZero(t *testing.T) {
	path, w := emptyDB(t, false)
	s, r := source(path, 20), &run{}
	r.collect(t, s, t0)
	insert(t, w, "c1", done("sub-provider", "model-y", 1, 1, 0, 0, 0, 0))
	got := r.collect(t, s, t0.Add(time.Minute))
	if _, ok := got["agent_cost_usd_total{sub-provider,model-y}"]; ok {
		t.Fatal("a zero cost must not be exported")
	}
	insert(t, w, "c2", done("sub-provider", "model-y", 1, 1, 0, 0, 0, 0.25))
	insert(t, w, "c3", done("sub-provider", "model-y", 1, 1, 0, 0, 0, 0))
	got = r.collect(t, s, t0.Add(2*time.Minute))
	if v, ok := got["agent_cost_usd_total{sub-provider,model-y}"]; !ok || v != 0.25 {
		t.Fatalf("cost after a non-zero report: %v %v", v, ok)
	}
}

// Rows present at first start, complete or not, are history: never counted, now or later.
func TestNoReplay(t *testing.T) {
	path, w := emptyDB(t, false)
	exec(t, w, "rows.sql")
	insert(t, w, "in-flight", streaming("provider-a", "model-y", 1))
	s, r := source(path, 20), &run{}
	if got := r.collect(t, s, t0); len(got) != 0 {
		t.Fatalf("history counted on first start: %v", got)
	}
	update(t, w, "in-flight", done("provider-a", "model-y", 1, 1, 0, 0, 0, 0))
	if got := r.collect(t, s, t0.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("history counted on the second cycle: %v", got)
	}
	insert(t, w, "new", done("provider-a", "model-y", 7, 0, 0, 0, 0, 0))
	eq(t, "only the new row", r.collect(t, s, t0.Add(2*time.Minute)),
		counters("provider-a,model-y", 7, 0, 0, 0, 0, 1, 0))
}

// A restarted exporter continues from its persisted state: totals carry over, the cursor resumes, and a message
// pending at shutdown counts once when it completes.
func TestRestartContinues(t *testing.T) {
	path, w := emptyDB(t, false)
	r := &run{}
	s1 := source(path, 20)
	r.collect(t, s1, t0)
	insert(t, w, "m1", done("provider-a", "model-y", 10, 1, 0, 0, 0, 0))
	insert(t, w, "m2", streaming("provider-a", "model-y", 1))
	r.collect(t, s1, t0.Add(time.Minute))

	// Restart: a new instance with only the persisted blob. Meanwhile m2 completed and m3 arrived.
	update(t, w, "m2", done("provider-a", "model-y", 20, 2, 0, 0, 0, 0))
	insert(t, w, "m3", done("provider-a", "model-y", 30, 3, 0, 0, 0, 0))
	s2 := source(path, 20)
	eq(t, "after restart", r.collect(t, s2, t0.Add(2*time.Minute)),
		counters("provider-a,model-y", 60, 6, 0, 0, 0, 3, 0))
}

// Models are capped per profile; the rest count into model="other" under their provider. A pair unused for a
// day gives its slot to a new one, and its series ends.
func TestModelCap(t *testing.T) {
	path, w := emptyDB(t, false)
	s, r := source(path, 2), &run{}
	r.collect(t, s, t0)
	insert(t, w, "a", done("provider-a", "model-1", 1, 0, 0, 0, 0, 0))
	insert(t, w, "b", done("provider-a", "model-2", 2, 0, 0, 0, 0, 0))
	insert(t, w, "c", done("provider-a", "model-3", 4, 0, 0, 0, 0, 0))
	insert(t, w, "d", done("provider-b", "model-1", 8, 0, 0, 0, 0, 0))
	insert(t, w, "e", done("provider-a", "Bad Model!", 16, 0, 0, 0, 0, 0))
	insert(t, w, "f", done("bad provider", "model-1", 32, 0, 0, 0, 0, 0))
	got := r.collect(t, s, t0.Add(time.Minute))
	eq(t, "cap", got, merge(
		counters("provider-a,model-1", 1, 0, 0, 0, 0, 1, 0),
		counters("provider-a,model-2", 2, 0, 0, 0, 0, 1, 0),
		counters("provider-a,other", 20, 0, 0, 0, 0, 2, 0), // model-3 folded, invalid model id
		counters("provider-b,other", 8, 0, 0, 0, 0, 1, 0),  // the cap counts (provider, model) pairs
		counters("other,other", 32, 0, 0, 0, 0, 1, 0),      // invalid provider; both slots taken
	))

	// A day later model-1 is still in use, model-2 is not: model-3 takes model-2's slot.
	insert(t, w, "g", done("provider-a", "model-1", 64, 0, 0, 0, 0, 0))
	r.collect(t, s, t0.Add(25*time.Hour))
	insert(t, w, "h", done("provider-a", "model-3", 128, 0, 0, 0, 0, 0))
	// Both slots are now in recent use, so a further new model folds; the cap still holds after an eviction.
	insert(t, w, "i", done("provider-a", "model-4", 256, 0, 0, 0, 0, 0))
	got = r.collect(t, s, t0.Add(25*time.Hour+time.Minute))
	if _, ok := got["agent_requests_total{provider-a,model-2,ok}"]; ok {
		t.Fatal("the evicted pair's series must end")
	}
	if got["agent_tokens_total{provider-a,model-3,input}"] != 128 || got["agent_tokens_total{provider-a,other,input}"] != 276 ||
		got["agent_tokens_total{provider-a,model-1,input}"] != 65 || len(s.st.Named) != 2 {
		t.Fatalf("after eviction: %v named %v", got, s.st.Named)
	}
}

func sidecars(path string) []string {
	var out []string
	for _, s := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(path + s); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// A WAL database no writer has open cannot have changed: the persisted counters are served, the cycle succeeds,
// and the exporter creates no -wal or -shm in opencode's directory. Once a writer is back, counting resumes.
func TestIdleServesPersisted(t *testing.T) {
	path, w := emptyDB(t, true)
	r := &run{}
	s := source(path, 20)
	r.collect(t, s, t0)
	insert(t, w, "m1", done("provider-a", "model-y", 10, 1, 0, 0, 0, 0.5))
	want := r.collect(t, s, t0.Add(time.Minute))
	if len(want) == 0 {
		t.Fatal("nothing counted while the writer was live")
	}
	w.Close() // opencode exits: SQLite checkpoints and removes both sidecars
	if sc := sidecars(path); len(sc) != 0 {
		t.Fatalf("precondition: sidecars after the writer closed: %v", sc)
	}

	// A restarted exporter with only the persisted blob serves the same counters.
	s2 := source(path, 20)
	res, st, err := s2.Collect(context.Background(), t0.Add(2*time.Minute), r.state)
	if err != nil {
		t.Fatalf("idle database must not fail the source: %v", err)
	}
	if st != nil {
		t.Fatal("idle cycle must leave the persisted state as it is")
	}
	eq(t, "idle", series(res), want)
	if sc := sidecars(path); len(sc) != 0 {
		t.Fatalf("the exporter created %v next to an idle database", sc)
	}

	w2 := writer(t, path, true)
	defer w2.Close()
	insert(t, w2, "m2", done("provider-a", "model-y", 5, 0, 0, 0, 0, 0))
	got := r.collect(t, s2, t0.Add(3*time.Minute))
	if got["agent_tokens_total{provider-a,model-y,input}"] != 15 {
		t.Fatalf("counting after the writer returned: %v", got)
	}
}

// A missing database is not_found and a locked one is timeout (retried next cycle): never zero values.
func TestFailures(t *testing.T) {
	s := source(filepath.Join(t.TempDir(), dbFile), 20)
	res, _, err := s.Collect(context.Background(), t0, nil)
	if reasonOf(err) != core.ReasonNotFound || len(res.Samples) != 0 {
		t.Fatalf("missing db: %v %v", err, res.Samples)
	}

	path, w := emptyDB(t, false)
	ctx := context.Background()
	lock, err := w.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	defer func() { lock.ExecContext(ctx, "ROLLBACK"); lock.Close() }()
	_, _, err = source(path, 20).Collect(ctx, t0, nil)
	if reasonOf(err) != core.ReasonTimeout {
		t.Fatalf("locked db: %v", err)
	}
}

func reasonOf(err error) string {
	var se *core.SourceError
	if errors.As(err, &se) {
		return se.Reason
	}
	return fmt.Sprintf("not a SourceError: %v", err)
}

// A message that never completes (its writer died) is dropped after a day, and the pending set is capped; each
// drop is a counted source error, never a silent loss.
func TestPendingBounded(t *testing.T) {
	path, w := emptyDB(t, false)
	s, r := source(path, 20), &run{}
	r.collect(t, s, t0)
	insert(t, w, "stuck", streaming("provider-a", "model-y", 1))
	r.collect(t, s, t0.Add(time.Minute))
	if len(s.st.Pending) != 1 {
		t.Fatalf("pending: %v", s.st.Pending)
	}
	r.collect(t, s, t0.Add(25*time.Hour))
	if len(s.st.Pending) != 0 || r.res.Errors[core.ReasonOther] != 1 {
		t.Fatalf("expiry: pending %d, errors %v", len(s.st.Pending), r.res.Errors)
	}

	tx, err := w.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < pendingCap+3; i++ {
		if _, err := tx.Exec(`INSERT INTO message VALUES (?, 'ses_00000000000000000001', 1, 1, ?)`,
			fmt.Sprintf("p%05d", i), streaming("provider-a", "model-y", 1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r.collect(t, s, t0.Add(26*time.Hour))
	if len(s.st.Pending) != pendingCap || r.res.Errors[core.ReasonOther] != 3 {
		t.Fatalf("cap: pending %d, errors %v", len(s.st.Pending), r.res.Errors)
	}
	if _, ok := s.st.Pending["p00000"]; ok {
		t.Fatal("the cap must drop the oldest")
	}
}

// If the last rowid falls below the cursor (database replaced), the cursor follows it without replay.
func TestCursorReset(t *testing.T) {
	path, w := emptyDB(t, false)
	s, r := source(path, 20), &run{}
	r.collect(t, s, t0)
	for _, id := range []string{"a", "b", "c"} {
		insert(t, w, id, done("provider-a", "model-y", 1, 0, 0, 0, 0, 0))
	}
	r.collect(t, s, t0.Add(time.Minute))
	exec(t, w, "DELETE FROM message")
	insert(t, w, "d", done("provider-a", "model-y", 100, 0, 0, 0, 0, 0)) // rowid 1, below the cursor
	r.collect(t, s, t0.Add(2*time.Minute))
	insert(t, w, "e", done("provider-a", "model-y", 10, 0, 0, 0, 0, 0))
	got := r.collect(t, s, t0.Add(3*time.Minute))
	if got["agent_tokens_total{provider-a,model-y,input}"] != 13 {
		t.Fatalf("after reset: %v", got)
	}
}

// No canary planted in message content, parts, session titles, todos, shares or account rows ever appears in
// samples, errors or the persisted state.
func TestCanaryNeverLeaves(t *testing.T) {
	path, w := emptyDB(t, false)
	exec(t, w, "rows.sql")
	s, r := source(path, 20), &run{}
	r.collect(t, s, t0)
	exec(t, w,
		`INSERT INTO account VALUES ('acc1','a@example.com','https://example.com','`+canary+`','`+canary+`',1,1,1)`,
		`INSERT INTO control_account VALUES ('a@example.com','https://example.com','`+canary+`','`+canary+`',1,1,1,1)`,
		`INSERT INTO credential VALUES ('cr1',NULL,'`+canary+`','`+canary+`',NULL,NULL,1,1,1)`,
		`INSERT INTO session_share VALUES ('ses_00000000000000000001','sh1','`+canary+`','https://example.com/`+canary+`',1,1)`,
		`INSERT INTO todo VALUES ('ses_00000000000000000001','`+canary+`','pending','high',0,1,1)`,
		`UPDATE session SET title = '`+canary+`', summary_diffs = '`+canary+`'`,
		`INSERT INTO part VALUES ('prt_1','msg_00000000000000000002','ses_00000000000000000001',1,1,
		 '{"type":"text","text":"`+canary+`"}')`,
		`INSERT INTO session_input VALUES ('in1','ses_00000000000000000001','`+canary+`','queued',1,NULL,1)`)
	// Content inside message.data: unlisted fields, an object under a listed path, an error message.
	insert(t, w, "k1", `{"role":"assistant","time":{"created":1,"completed":2},"providerID":"provider-a",`+
		`"modelID":{"secret":"`+canary+`"},"summary":{"title":"`+canary+`","body":"`+canary+`"},"text":"`+canary+`",`+
		`"path":{"cwd":"/root/`+canary+`"},"tokens":{"input":1,"output":{"x":"`+canary+`"}},"cost":0}`)
	insert(t, w, "k2", failed("APIError", 429))
	insert(t, w, "k3", `{"role":"user","content":"`+canary+`"}`)
	insert(t, w, "k4", `not json `+canary)
	r.collect(t, s, t0.Add(time.Minute))

	blob := fmt.Sprintf("%+v %v %s", r.res.Samples, r.res.Errors, r.state)
	if strings.Contains(strings.ToLower(blob), strings.ToLower(canary)) {
		t.Fatal("canary leaked")
	}
	got := series(r.res)
	if got["agent_tokens_total{provider-a,unknown,input}"] != 1 || r.res.Errors[core.ReasonParse] != 1 {
		t.Fatalf("hostile rows: %v errors %v", got, r.res.Errors)
	}
}

// The allow-list lists only message.id and scalars of message.data. A query this plugin could be changed to make
// with any other expression is refused before SQL is built.
func TestAllowRefusesEverythingElse(t *testing.T) {
	if len(allow) != 1 || allow[table] == nil {
		t.Fatalf("only the message table may be listed: %v", allow)
	}
	for _, e := range allow[table] {
		if c, ok := e.(sqlro.Col); ok && c != "id" {
			t.Fatalf("bare column %q listed: only id may be read whole", c)
		}
		if j, ok := e.(sqlro.JSON); ok && j.Col != "data" {
			t.Fatalf("JSON of %q listed", j.Col)
		}
	}

	path, w := emptyDB(t, false)
	exec(t, w, "rows.sql")
	db, err := sqlro.Open(path, allow)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if rows, err := db.After(table, "rowid", 0, cols, 10); err != nil || len(rows) != 3 {
		t.Fatalf("the plugin's own query must work: %v %d", err, len(rows))
	}
	type q struct {
		table string
		e     sqlro.Expr
	}
	for _, c := range []q{
		{"message", sqlro.Col("data")},
		{"message", sqlro.Col("session_id")},
		{"message", sqlro.JSON{Col: "data", Path: "$.error.data.message"}},
		{"message", sqlro.JSON{Col: "data", Path: "$.summary"}},
		{"message", sqlro.JSON{Col: "data", Path: "$.tokens"}},
		{"part", sqlro.Col("data")},
		{"part", sqlro.Col("id")},
		{"part", sqlro.JSON{Col: "data", Path: "$.text"}},
		{"session", sqlro.Col("title")},
		{"session", sqlro.Col("id")},
		{"account", sqlro.Col("access_token")},
		{"control_account", sqlro.Col("refresh_token")},
		{"credential", sqlro.Col("value")},
		{"session_share", sqlro.Col("secret")},
		{"todo", sqlro.Col("content")},
		{"event", sqlro.Col("data")},
	} {
		if _, err := db.After(c.table, "rowid", 0, []sqlro.Expr{c.e}, 10); reasonOf(err) != core.ReasonOther {
			t.Errorf("After %s %v: want refusal, got %v", c.table, c.e, err)
		}
		if _, err := db.ByKeys(c.table, "id", []string{"x"}, []sqlro.Expr{c.e}); reasonOf(err) != core.ReasonOther {
			t.Errorf("ByKeys %s %v: want refusal, got %v", c.table, c.e, err)
		}
	}
	for _, tbl := range []string{"part", "session", "account", "credential"} {
		if _, err := db.MaxRowid(tbl); reasonOf(err) != core.ReasonOther {
			t.Errorf("MaxRowid %s: want refusal, got %v", tbl, err)
		}
	}
}
