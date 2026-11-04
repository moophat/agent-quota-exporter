package sqlro

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// canary is planted in every secret column of the fixtures; no allowed query may ever return it.
const canary = "sk-CANARY-5e1f0c9d"

var opencodeAllow = Allow{
	"message": {Col("id"), Col("session_id"), Col("time_created"),
		JSON{"data", "$.role"}, JSON{"data", "$.providerID"}, JSON{"data", "$.modelID"}, JSON{"data", "$.cost"},
		JSON{"data", "$.tokens.input"}, JSON{"data", "$.tokens.output"}, JSON{"data", "$.tokens.cache.read"},
		JSON{"data", "$.finish"}, JSON{"data", "$.time.completed"}, JSON{"data", "$.error.name"}},
	"session": {Col("id"), Col("model"), Col("tokens_input"), Col("cost")},
}

var msgCols = []Expr{Col("id"), JSON{"data", "$.role"}, JSON{"data", "$.providerID"}, JSON{"data", "$.modelID"},
	JSON{"data", "$.tokens.input"}, JSON{"data", "$.cost"}, JSON{"data", "$.finish"}, JSON{"data", "$.error.name"},
	Col("time_created")}

var routerAllow = Allow{
	"providerConnections": {Col("id"), Col("provider"), Col("isActive"), JSON{"data", "$.testStatus"},
		JSON{"data", "$.errorCode"}, JSON{"data", "$.rateLimitedUntil"}, JSON{"data", "$.lastErrorAt"}},
	"usageHistory": {Col("id"), Col("provider"), Col("model"), Col("connectionId"), Col("promptTokens"),
		Col("completionTokens"), Col("status")},
}

var connCols = []Expr{Col("id"), JSON{"data", "$.testStatus"}, JSON{"data", "$.errorCode"},
	JSON{"data", "$.rateLimitedUntil"}, JSON{"data", "$.lastErrorAt"}}

var usageCols = []Expr{Col("id"), Col("provider"), Col("model"), Col("promptTokens"), Col("completionTokens")}

var long = strings.Repeat("x", 300)

// writer opens a read-write connection the way the owning program would.
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

func execAll(t *testing.T, w *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if strings.HasSuffix(s, ".sql") {
			b, err := os.ReadFile(filepath.Join("..", "..", "fixtures", s))
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

// opencodeDB builds the opencode fixture plus secret rows and hostile message rows, writer closed.
func opencodeDB(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "opencode.db")
	w := writer(t, path, false)
	defer w.Close()
	execAll(t, w, "opencode/opencode.schema.sql", "opencode/rows.sql",
		`INSERT INTO account VALUES ('acc1','a@example.com','https://example.com','`+canary+`','`+canary+`',1,1,1)`,
		`INSERT INTO control_account VALUES ('a@example.com','https://example.com','`+canary+`','`+canary+`',1,1,1,1)`,
		`INSERT INTO credential VALUES ('cr1',NULL,'l','`+canary+`',NULL,NULL,1,1,1)`,
		// An object under a listed path must come back NULL, and long text must be capped.
		`INSERT INTO message VALUES ('msg_00000000000000000004','ses_00000000000000000001',1767225700000,1767225700000,
		 '{"role":"assistant","providerID":"provider-a","modelID":{"secret":"`+canary+`"},"finish":"`+long+`",
		   "tokens":{"input":7,"output":1},"cost":0.5}')`)
	return path
}

// routerDB builds the router fixture; the writer is returned open so a test can keep WAL files alive.
func routerDB(t *testing.T, dir string, wal bool) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(dir, "data.sqlite")
	w := writer(t, path, wal)
	execAll(t, w, "router/router.schema.sql",
		`INSERT INTO providerConnections VALUES
		 ('c1','provider-a','oauth','n1','a@example.com',1,1,
		  '{"accessToken":"`+canary+`","refreshToken":"`+canary+`","testStatus":"active","errorCode":null,"rateLimitedUntil":"2026-11-04T01:00:00Z","lastErrorAt":"2026-11-04T00:00:00Z"}','t','t'),
		 ('c2','provider-b','apikey','n2','b@example.com',2,1,
		  '{"apiKey":"`+canary+`","testStatus":"`+long+`","errorCode":429,"lastErrorAt":{"detail":"`+canary+`"}}','t','t'),
		 ('c3','provider-c','apikey','n3','c@example.com',3,0,'not json `+canary+`','t','t')`,
		`INSERT INTO usageHistory (id,timestamp,provider,model,connectionId,apiKey,promptTokens,completionTokens,status) VALUES
		 (1,'2026-11-04T00:00:00Z','provider-a','model-y','c1','`+canary+`',100,10,'ok'),
		 (2,'2026-11-04T00:01:00Z','provider-a','`+long+`','c1','`+canary+`',200,20,'ok'),
		 (3,'2026-11-04T00:02:00Z','provider-b','model-z','c2','`+canary+`',300,30,'error')`)
	return path, w
}

// lockExclusive holds an EXCLUSIVE lock on w's (rollback-journal) database until the test ends.
func lockExclusive(t *testing.T, w *sql.DB) {
	t.Helper()
	ctx := context.Background()
	lock, err := w.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		lock.ExecContext(ctx, "ROLLBACK")
		lock.Close()
	})
}

func reasonOf(err error) string {
	var se *core.SourceError
	if errors.As(err, &se) {
		return se.Reason
	}
	return fmt.Sprintf("not a SourceError: %T", err)
}

func mustOpen(t *testing.T, path string, a Allow) *DB {
	t.Helper()
	d, err := Open(path, a)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func noCanary(t *testing.T, v any) {
	t.Helper()
	if s := fmt.Sprintf("%#v", v); strings.Contains(s, canary) {
		t.Fatalf("secret canary leaked: %s", s)
	}
}

func TestOpencodeAllowedQueries(t *testing.T) {
	d := mustOpen(t, opencodeDB(t, t.TempDir()), opencodeAllow)
	rows, err := d.After("message", "rowid", 0, msgCols, 100)
	if err != nil || len(rows) != 4 {
		t.Fatalf("rows %d err %v", len(rows), err)
	}
	noCanary(t, rows)
	// The assistant message: every metric field comes out typed, nothing else of the JSON does.
	want := []any{"msg_00000000000000000002", "assistant", "provider-a", "model-y", int64(24545), 0.0037, "stop", nil,
		int64(1767225605000)}
	if got := fmt.Sprint(rows[1]); got != fmt.Sprint(want) {
		t.Fatalf("message row\n got %v\nwant %v", got, want)
	}
	if rows[0][1] != "user" || rows[0][4] != nil || rows[2][7] != "APIError" {
		t.Fatalf("user/error rows: %v / %v", rows[0], rows[2])
	}
	// An object under a listed path is NULL (it held the canary); long text is capped at 128 characters.
	if rows[3][3] != nil || rows[3][6] != long[:maxText] {
		t.Fatalf("object or long text not neutralised: %v", rows[3])
	}

	// The rowid cursor resumes after the last row read.
	rest, err := d.After("message", "rowid", 2, []Expr{Col("rowid"), Col("id")}, 100)
	if err != nil || len(rest) != 2 || rest[0][0] != int64(3) {
		t.Fatalf("cursor: %v %v", rest, err)
	}
	byKey, err := d.ByKeys("message", "id", []string{"msg_00000000000000000003", "msg_missing"}, msgCols)
	if err != nil || len(byKey) != 1 || byKey[0][7] != "APIError" {
		t.Fatalf("by key: %v %v", byKey, err)
	}
	if none, err := d.ByKeys("message", "id", nil, msgCols); err != nil || none != nil {
		t.Fatalf("no keys: %v %v", none, err)
	}
	if n, err := d.MaxRowid("message"); err != nil || n != 4 {
		t.Fatalf("max rowid %d %v", n, err)
	}
	if n, err := d.MaxInt("message", "time_created"); err != nil || n != 1767225700000 {
		t.Fatalf("max int %d %v", n, err)
	}
	if _, err := d.MaxInt("session", "model"); reasonOf(err) != core.ReasonOther {
		t.Fatalf("non-integer max must fail, got %v", err)
	}
	sess, err := d.After("session", "rowid", 0, []Expr{Col("id"), Col("model"), Col("tokens_input"), Col("cost")}, 10)
	if err != nil || fmt.Sprint(sess) != "[[ses_00000000000000000001 provider-a/model-y 24545 0.0111]]" {
		t.Fatalf("session: %v %v", sess, err)
	}
}

func TestRouterAllowedQueries(t *testing.T) {
	path, w := routerDB(t, t.TempDir(), false)
	w.Close()
	d := mustOpen(t, path, routerAllow)
	conns, err := d.After("providerConnections", "rowid", 0, connCols, 50)
	if err != nil || len(conns) != 3 {
		t.Fatalf("conns %v %v", conns, err)
	}
	noCanary(t, conns)
	if got := fmt.Sprint(conns[0]); got != "[c1 active <nil> 2026-11-04T01:00:00Z 2026-11-04T00:00:00Z]" {
		t.Fatalf("c1: %s", got)
	}
	// c2: capped text, integer error code, object -> NULL. c3: malformed JSON -> all NULL, no query failure.
	if conns[1][1] != long[:maxText] || conns[1][2] != int64(429) || conns[1][4] != nil {
		t.Fatalf("c2: %v", conns[1])
	}
	if fmt.Sprint(conns[2]) != "[c3 <nil> <nil> <nil> <nil>]" {
		t.Fatalf("c3: %v", conns[2])
	}

	usage, err := d.After("usageHistory", "id", 1, usageCols, 10)
	if err != nil || len(usage) != 2 || usage[0][0] != int64(2) || usage[1][0] != int64(3) {
		t.Fatalf("usage: %v %v", usage, err)
	}
	if usage[0][2] != long[:maxText] || usage[1][3] != int64(300) {
		t.Fatalf("plain text cap or ints: %v", usage)
	}
	noCanary(t, usage)
	if page, err := d.After("usageHistory", "id", 0, usageCols, 1); err != nil || len(page) != 1 {
		t.Fatalf("limit: %v %v", page, err)
	}
	byKey, err := d.ByKeys("usageHistory", "connectionId", []string{"c2"}, usageCols)
	if err != nil || len(byKey) != 1 || byKey[0][0] != int64(3) {
		t.Fatalf("by key: %v %v", byKey, err)
	}
	noCanary(t, byKey)
	if n, err := d.MaxInt("usageHistory", "id"); err != nil || n != 3 {
		t.Fatalf("max id %d %v", n, err)
	}
}

// Forbidden selections must fail before any SQL runs. The database is held under an EXCLUSIVE lock while they
// are tried: any statement that reached SQLite would come back as a timeout, so a fast ReasonOther proves the
// request was refused by the allow-list itself.
func TestForbiddenFailsBeforeSQL(t *testing.T) {
	dir := t.TempDir()
	rPath, w := routerDB(t, dir, false)
	defer w.Close()
	oPath := opencodeDB(t, dir)
	hostile := Allow{ // even when listed, malformed identifiers and paths are refused
		"usageHistory":    {Col(`id" , "apiKey`), Col("id x"), JSON{"meta", "$.a') --"}, JSON{"meta", "$"}, Col("id")},
		`usageHistory"--`: {Col("id")},
	}
	r, o, h := mustOpen(t, rPath, routerAllow), mustOpen(t, oPath, opencodeAllow), mustOpen(t, rPath, hostile)

	ow := writer(t, oPath, false)
	defer ow.Close()
	lockExclusive(t, w)
	lockExclusive(t, ow)
	if _, err := o.MaxRowid("message"); reasonOf(err) != core.ReasonTimeout {
		t.Fatalf("control: an allowed query under the lock must time out, got %v", err)
	}
	if _, err := r.After("usageHistory", "id", 0, usageCols, 10); reasonOf(err) != core.ReasonTimeout {
		t.Fatalf("control: an allowed query under the lock must time out, got %v", err)
	}

	tooMany := make([]string, maxKeys+1)
	cases := map[string]func() error{
		"usageHistory.apiKey":      func() error { _, e := r.After("usageHistory", "id", 0, []Expr{Col("apiKey")}, 10); return e },
		"providerConnections.data": func() error { _, e := r.After("providerConnections", "rowid", 0, []Expr{Col("data")}, 10); return e },
		"unlisted json path": func() error {
			_, e := r.After("providerConnections", "rowid", 0, []Expr{JSON{"data", "$.accessToken"}}, 10)
			return e
		},
		"json path on unlisted col":  func() error { _, e := r.After("usageHistory", "id", 0, []Expr{JSON{"apiKey", "$.x"}}, 10); return e },
		"allowed then forbidden col": func() error { _, e := r.After("usageHistory", "id", 0, []Expr{Col("id"), Col("apiKey")}, 10); return e },
		"cursor on secret col":       func() error { _, e := r.After("usageHistory", "apiKey", 0, usageCols, 10); return e },
		"key on secret col":          func() error { _, e := r.ByKeys("usageHistory", "apiKey", []string{canary}, usageCols); return e },
		"max of secret col":          func() error { _, e := r.MaxInt("usageHistory", "apiKey"); return e },
		"unlisted table":             func() error { _, e := r.MaxRowid("sqlite_master"); return e },
		"account.access_token":       func() error { _, e := o.After("account", "rowid", 0, []Expr{Col("access_token")}, 10); return e },
		"account.id":                 func() error { _, e := o.After("account", "rowid", 0, []Expr{Col("id")}, 10); return e },
		"account rowid":              func() error { _, e := o.MaxRowid("account"); return e },
		"credential.value":           func() error { _, e := o.ByKeys("credential", "id", []string{"cr1"}, []Expr{Col("value")}); return e },
		"control_account.email":      func() error { _, e := o.After("control_account", "rowid", 0, []Expr{Col("email")}, 10); return e },
		"message.data whole":         func() error { _, e := o.After("message", "rowid", 0, []Expr{Col("data")}, 10); return e },
		"listed ident with quote":    func() error { _, e := h.After("usageHistory", "id", 0, []Expr{Col(`id" , "apiKey`)}, 10); return e },
		"listed ident with space":    func() error { _, e := h.After("usageHistory", "id", 0, []Expr{Col("id x")}, 10); return e },
		"listed path injection":      func() error { _, e := h.After("usageHistory", "id", 0, []Expr{JSON{"meta", "$.a') --"}}, 10); return e },
		"listed root path":           func() error { _, e := h.After("usageHistory", "id", 0, []Expr{JSON{"meta", "$"}}, 10); return e },
		"listed table with quote":    func() error { _, e := h.MaxRowid(`usageHistory"--`); return e },
		"nil expression":             func() error { _, e := r.After("usageHistory", "id", 0, []Expr{nil}, 10); return e },
		"no columns":                 func() error { _, e := r.After("usageHistory", "id", 0, nil, 10); return e },
		"limit zero":                 func() error { _, e := r.After("usageHistory", "id", 0, usageCols, 0); return e },
		"limit too large":            func() error { _, e := r.After("usageHistory", "id", 0, usageCols, maxLimit+1); return e },
		"too many keys":              func() error { _, e := r.ByKeys("usageHistory", "connectionId", tooMany, usageCols); return e },
	}
	for name, run := range cases {
		start := time.Now()
		err := run()
		if reasonOf(err) != core.ReasonOther || time.Since(start) > 100*time.Millisecond {
			t.Errorf("%s: want fast %s, got %v after %v", name, core.ReasonOther, err, time.Since(start))
		}
		if strings.Contains(err.Error(), "usageHistory") || strings.Contains(err.Error(), "SELECT") {
			t.Errorf("%s: error text leaks query detail: %q", name, err)
		}
	}
}

// A database whose owner holds a lock is a timeout to retry next cycle, never a wait on the owner.
func TestBusyMapsToTimeout(t *testing.T) {
	path, w := routerDB(t, t.TempDir(), false)
	defer w.Close()
	lockExclusive(t, w)
	start := time.Now()
	_, err := Open(path, routerAllow)
	if reasonOf(err) != core.ReasonTimeout {
		t.Fatalf("want timeout, got %v", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("busy wait too long: %v", el)
	}
}

func TestOpenRefusesLinksAndNonFiles(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	path := opencodeDB(t, real)
	if err := os.Symlink(real, filepath.Join(dir, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(dir, "link.db")); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{
		"link in a directory component": filepath.Join(dir, "linkdir", "opencode.db"),
		"link as the file":              filepath.Join(dir, "link.db"),
		"directory":                     real,
	} {
		if _, err := Open(p, opencodeAllow); reasonOf(err) != core.ReasonPermission {
			t.Errorf("%s: want %s, got %v", name, core.ReasonPermission, err)
		}
	}
	if _, err := Open(filepath.Join(dir, "missing.db"), opencodeAllow); reasonOf(err) != core.ReasonNotFound {
		t.Errorf("missing: got %v", err)
	}
	notDB := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(notDB, []byte(strings.Repeat("not a database ", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(notDB, opencodeAllow); reasonOf(err) != core.ReasonOther {
		t.Errorf("not a database: got %v", err)
	}
	mustOpen(t, path, opencodeAllow) // the real path still opens
}
