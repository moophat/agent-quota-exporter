package zai

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/sqlro"
)

const (
	keyA = "AAAAAAAAAAAAAAAAAAAA"
	keyB = "BBBBBBBBBBBBBBBBBBBB"
	keyC = "CCCCCCCCCCCCCCCCCCCC"
)

func openWriter(t *testing.T, path string, wal bool) *sql.DB {
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

// credDB builds <dir>/opencode.db from the fixture DDL; the writer is returned open.
func credDB(t *testing.T, dir string, wal bool) *sql.DB {
	t.Helper()
	w := openWriter(t, filepath.Join(dir, "opencode.db"), wal)
	t.Cleanup(func() { w.Close() })
	if _, err := w.Exec(fixtureBody(t, "opencode-credential.sql")); err != nil {
		t.Fatal(err)
	}
	return w
}

var credN int

func addCred(t *testing.T, w *sql.DB, integration, value string, active any, updated int64) {
	t.Helper()
	credN++
	if _, err := w.Exec(`INSERT INTO credential (id, integration_id, label, value, active, time_created, time_updated) VALUES (?,?,?,?,?,?,?)`,
		"cred_"+strings.Repeat("0", 20)+string(rune('a'+credN%26))+string(rune('a'+credN/26)), integration, "l", value, active, updated, updated); err != nil {
		t.Fatal(err)
	}
}

func keyJSON(k string) string { return `{"type":"key","key":"` + k + `"}` }

func TestOpencodeAuthJSON(t *testing.T) {
	d := holderDir(t, "auth.json", fixtureBody(t, "opencode-auth.json"))
	k, err := readOpencode(d)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Zero()
	if string(k.value) != "CANARYZAI0000000000000000.CANARYAUTHGLOBAL" {
		t.Fatal("zai-coding-plan must win over zhipuai-coding-plan")
	}
	if k.fp == "" {
		t.Fatal("no fp")
	}
}

func TestOpencodeAuthJSONSelection(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"cn only":          {`{"zhipuai-coding-plan":{"type":"api","key":"` + keyB + `"}}`, keyB},
		"oauth skipped":    {`{"zai-coding-plan":{"type":"oauth","key":"` + keyA + `"},"zai":{"type":"api","key":"` + keyB + `"}}`, keyB},
		"wellknown":        {`{"zai-coding-plan":{"type":"wellknown","key":"` + keyA + `"},"zhipuai":{"type":"api","key":"` + keyC + `"}}`, keyC},
		"trailing slash":   {`{"zai-coding-plan/":{"type":"api","key":"` + keyA + `"}}`, keyA},
		"order zai before": {`{"zhipuai":{"type":"api","key":"` + keyC + `"},"zai":{"type":"api","key":"` + keyB + `"}}`, keyB},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			k, err := readOpencode(holderDir(t, "auth.json", tc.body))
			if err != nil {
				t.Fatal(err)
			}
			defer k.Zero()
			if string(k.value) != tc.want {
				t.Fatal("wrong entry")
			}
		})
	}
}

func TestOpencodeDB(t *testing.T) {
	t.Run("single row", func(t *testing.T) {
		d := t.TempDir()
		w := credDB(t, d, false)
		addCred(t, w, "zai-coding-plan", keyJSON(keyA), 1, 100)
		k, err := readOpencode(d)
		if err != nil {
			t.Fatal(err)
		}
		defer k.Zero()
		if string(k.value) != keyA {
			t.Fatal("wrong key")
		}
		if k.fp != "db:100:zai-coding-plan" {
			t.Fatalf("fp %q", k.fp)
		}
	})
	t.Run("active wins over newer inactive", func(t *testing.T) {
		d := t.TempDir()
		w := credDB(t, d, false)
		addCred(t, w, "zai-coding-plan", keyJSON(keyA), 1, 100)
		addCred(t, w, "zai-coding-plan", keyJSON(keyB), 0, 900)
		k, err := readOpencode(d)
		if err != nil {
			t.Fatal(err)
		}
		defer k.Zero()
		if string(k.value) != keyA {
			t.Fatal("inactive row chosen")
		}
	})
	t.Run("no active: newest wins", func(t *testing.T) {
		d := t.TempDir()
		w := credDB(t, d, false)
		addCred(t, w, "zai-coding-plan", keyJSON(keyA), 0, 100)
		addCred(t, w, "zai-coding-plan", keyJSON(keyB), nil, 900)
		k, err := readOpencode(d)
		if err != nil {
			t.Fatal(err)
		}
		defer k.Zero()
		if string(k.value) != keyB {
			t.Fatal("older row chosen")
		}
	})
	t.Run("integration order beats activity", func(t *testing.T) {
		d := t.TempDir()
		w := credDB(t, d, false)
		addCred(t, w, "zhipuai-coding-plan", keyJSON(keyB), 1, 900)
		addCred(t, w, "zai-coding-plan", keyJSON(keyA), 0, 100)
		k, err := readOpencode(d)
		if err != nil {
			t.Fatal(err)
		}
		defer k.Zero()
		if string(k.value) != keyA {
			t.Fatal("order ignored")
		}
	})
	t.Run("oauth external and bad json skipped", func(t *testing.T) {
		d := t.TempDir()
		w := credDB(t, d, false)
		addCred(t, w, "zai-coding-plan", `{"type":"oauth","key":"`+keyA+`"}`, 1, 300)
		addCred(t, w, "zai-coding-plan", `{"type":"external","key":"`+keyA+`"}`, 1, 400)
		addCred(t, w, "zai-coding-plan", `not json at all`, 1, 500)
		addCred(t, w, "zai", keyJSON(keyC), 1, 100)
		k, err := readOpencode(d)
		if err != nil {
			t.Fatal(err)
		}
		defer k.Zero()
		if string(k.value) != keyC {
			t.Fatal("wrong row")
		}
	})
	t.Run("no matching row", func(t *testing.T) {
		d := t.TempDir()
		credDB(t, d, false)
		_, err := readOpencode(d)
		wantReason(t, err, core.ReasonNotFound)
	})
	t.Run("bad key shape", func(t *testing.T) {
		d := t.TempDir()
		w := credDB(t, d, false)
		addCred(t, w, "zai-coding-plan", keyJSON("short"), 1, 100)
		_, err := readOpencode(d)
		wantReason(t, err, core.ReasonParse)
	})
	t.Run("idle wal database", func(t *testing.T) {
		d := t.TempDir()
		w := credDB(t, d, true)
		addCred(t, w, "zai-coding-plan", keyJSON(keyA), 1, 100)
		w.Close() // last close checkpoints and removes both sidecars
		for _, s := range []string{"-wal", "-shm"} {
			if _, err := os.Stat(filepath.Join(d, "opencode.db"+s)); err == nil {
				t.Skipf("sidecar %s left behind", s)
			}
		}
		k, err := readOpencode(d)
		if err != nil {
			t.Fatal(err)
		}
		defer k.Zero()
		if string(k.value) != keyA {
			t.Fatal("wrong key")
		}
	})
	t.Run("locked database", func(t *testing.T) {
		d := t.TempDir()
		w := credDB(t, d, false)
		addCred(t, w, "zai-coding-plan", keyJSON(keyA), 1, 100)
		conn, err := w.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(t.Context(), "BEGIN EXCLUSIVE"); err != nil {
			t.Fatal(err)
		}
		_, err = readOpencode(d)
		wantReason(t, err, core.ReasonTimeout)
	})
	t.Run("database without credential table", func(t *testing.T) {
		d := t.TempDir()
		w := openWriter(t, filepath.Join(d, "opencode.db"), false)
		defer w.Close()
		if _, err := w.Exec("CREATE TABLE message (id text)"); err != nil {
			t.Fatal(err)
		}
		_, err := readOpencode(d)
		wantReason(t, err, core.ReasonNotFound)
	})
}

func TestOpencodeMissing(t *testing.T) {
	_, err := readOpencode(t.TempDir())
	wantReason(t, err, core.ReasonNotFound)
	_, err = readOpencode(filepath.Join(t.TempDir(), "absent"))
	wantReason(t, err, core.ReasonNotFound)
}

func TestOpencodeOrder(t *testing.T) {
	t.Run("auth.json wins over the db", func(t *testing.T) {
		d := holderDir(t, "auth.json", `{"zai-coding-plan":{"type":"api","key":"`+keyA+`"}}`)
		w := credDB(t, d, false)
		addCred(t, w, "zai-coding-plan", keyJSON(keyB), 1, 100)
		k, err := readOpencode(d)
		if err != nil {
			t.Fatal(err)
		}
		defer k.Zero()
		if string(k.value) != keyA {
			t.Fatal("db used despite auth.json match")
		}
	})
	t.Run("db used when auth.json has no match", func(t *testing.T) {
		d := holderDir(t, "auth.json", `{"other":{"type":"api","key":"`+keyA+`"}}`)
		w := credDB(t, d, false)
		addCred(t, w, "zai-coding-plan", keyJSON(keyB), 1, 100)
		k, err := readOpencode(d)
		if err != nil {
			t.Fatal(err)
		}
		defer k.Zero()
		if string(k.value) != keyB {
			t.Fatal("db not used")
		}
	})
	t.Run("malformed auth.json falls through to the db", func(t *testing.T) {
		d := holderDir(t, "auth.json", `{ nope`)
		w := credDB(t, d, false)
		addCred(t, w, "zai-coding-plan", keyJSON(keyB), 1, 100)
		k, err := readOpencode(d)
		if err != nil {
			t.Fatal(err)
		}
		k.Zero()
	})
	t.Run("malformed auth.json and no db is a parse failure", func(t *testing.T) {
		_, err := readOpencode(holderDir(t, "auth.json", `{ nope`))
		wantReason(t, err, core.ReasonParse)
	})
}

func TestOpencodeAllowList(t *testing.T) {
	want := sqlro.Allow{"credential": {
		sqlro.Col("integration_id"), sqlro.Col("active"), sqlro.Col("time_updated"),
		sqlro.JSON{Col: "value", Path: "$.type"}, sqlro.JSONMax{Col: "value", Path: "$.key", Max: 513},
	}}
	if !reflect.DeepEqual(credentialAllow, want) {
		t.Fatalf("allow-list changed: %v", credentialAllow)
	}
	d := t.TempDir()
	w := credDB(t, d, false)
	addCred(t, w, "zai-coding-plan", keyJSON(keyA), 1, 100)
	db, err := sqlro.Open(filepath.Join(d, "opencode.db"), credentialAllow)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ByKeys("credential", "integration_id", []string{"zai"}, []sqlro.Expr{sqlro.Col("value")}); err == nil {
		t.Fatal("selecting the whole value column must be rejected")
	}
	if _, err := db.ByKeys("credential", "integration_id", []string{"zai"}, []sqlro.Expr{sqlro.JSON{Col: "value", Path: "$.refresh"}}); err == nil {
		t.Fatal("unlisted json path must be rejected")
	}
}

func TestOpencodeErrorsLeakNothing(t *testing.T) {
	d := t.TempDir()
	w := credDB(t, d, false)
	addCred(t, w, "zai-coding-plan", keyJSON("has space in the key here"), 1, 100)
	_, err := readOpencode(d)
	if err == nil {
		t.Fatal("want error")
	}
	if s := err.Error(); strings.Contains(s, "has space") || strings.Contains(s, d) {
		t.Fatalf("leak: %s", s)
	}
}

// sqlro cuts text at 128 characters; the key scalar is read in full up to the 512-byte limit.
func TestOpencodeDBKeyLengths(t *testing.T) {
	for name, tc := range map[string]struct {
		n      int
		reason string
	}{"127": {127, ""}, "128": {128, ""}, "300": {300, ""}, "512": {512, ""}, "513": {513, core.ReasonParse}} {
		t.Run(name, func(t *testing.T) {
			d := t.TempDir()
			w := credDB(t, d, false)
			want := strings.Repeat("K", tc.n)
			addCred(t, w, "zai-coding-plan", keyJSON(want), 1, 100)
			k, err := readOpencode(d)
			if tc.reason != "" {
				wantReason(t, err, tc.reason)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer k.Zero()
			if string(k.value) != want {
				t.Fatalf("key cut to %d bytes", len(k.value))
			}
		})
	}
}
