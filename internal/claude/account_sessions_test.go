package claude

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The account label must be derived from the account id, stable across runs and short; the same account
// must get the same label on every node so the fleet can join on it.
func TestAccountLabelFromSiblingClaudeJSON(t *testing.T) {
	p := newProfile(t, "a")
	label, uuid := p.Account()
	if uuid != "00000000-0000-4000-8000-0000000000a1" || len(label) != 13 || label[:5] != "acct-" {
		t.Fatalf("label %q uuid %q", label, uuid)
	}
	q := newProfile(t, "b")
	if l2, _ := q.Account(); l2 != label {
		t.Fatalf("same account, different labels: %s %s", label, l2)
	}
}

func TestAccountAliasAndEmailModes(t *testing.T) {
	p := newProfile(t, "a")
	p.AccountAlias = "acct-a1"
	if l, _ := p.Account(); l != "acct-a1" {
		t.Fatalf("config alias must win, got %s", l)
	}
	q := newProfile(t, "b")
	if l, _ := q.Account(); l == "user-a1@example.com" {
		t.Fatal("email used without account_label: email")
	}
	r := newProfile(t, "c")
	r.EmailLabel = true
	if l, _ := r.Account(); l != "user-a1@example.com" {
		t.Fatalf("email mode: %s", l)
	}
}

// A CLAUDE_CONFIG_DIR keeps .claude.json inside the dir; that one wins over a sibling.
func TestClaudeJSONInsideConfigDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acct-x")
	write(t, filepath.Join(dir, ".claude.json"), []byte(`{"oauthAccount":{"accountUuid":"u-2"}}`))
	p := &Profile{Alias: "x", Dir: dir}
	if _, uuid := p.Account(); uuid != "u-2" {
		t.Fatalf("uuid %q", uuid)
	}
}

func TestAccountSourceLoggedInAndExpiry(t *testing.T) {
	p := newProfile(t, "a")
	s := &AccountSource{P: p, Interval: time.Minute}
	label, _ := p.Account()
	res, _, err := collect(t, s, t0, nil) // token expires 02:00, now 00:00
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := find(res.Samples, "agent_account_logged_in", "account", label, "provider", "anthropic"); !ok || v != 1 {
		t.Fatalf("logged_in %v %v", v, ok)
	}
	// expiresAt is Unix milliseconds in the file and must be exported in seconds.
	if v, _ := find(res.Samples, "agent_credential_expiry_timestamp_seconds"); v != 1767232800 {
		t.Fatalf("expiry %v", v)
	}
	if v, ok := find(res.Samples, "agent_account_info", "plan", "unknown"); !ok || v != 1 {
		t.Fatal("account_info missing")
	}
	noCanary(t, res.Samples, err)

	res, _, _ = collect(t, s, t0.Add(3*time.Hour), nil)
	if v, _ := find(res.Samples, "agent_account_logged_in"); v != 0 {
		t.Fatal("expired token must read as not logged in")
	}
}

func TestAccountSourceLoggedOut(t *testing.T) {
	p := newProfile(t, "a")
	os.Remove(filepath.Join(p.Dir, ".credentials.json"))
	res, _, err := collect(t, &AccountSource{P: p, Interval: time.Minute}, t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := find(res.Samples, "agent_account_logged_in"); !ok || v != 0 {
		t.Fatal("missing credentials must be logged_in 0, not an error")
	}
	if _, ok := find(res.Samples, "agent_credential_expiry_timestamp_seconds"); ok {
		t.Fatal("expiry must be absent when unknown")
	}
}

// A symlinked or non-regular credential file is refused: the exporter runs as root over user-owned dirs.
func TestSymlinkedCredentialsRefused(t *testing.T) {
	p := newProfile(t, "a")
	other := filepath.Join(t.TempDir(), "root-creds.json")
	write(t, other, fixture(t, "dot-credentials.json"))
	cp := filepath.Join(p.Dir, ".credentials.json")
	os.Remove(cp)
	if err := os.Symlink(other, cp); err != nil {
		t.Fatal(err)
	}
	_, _, err := collect(t, &AccountSource{P: p, Interval: time.Minute}, t0, nil)
	if err == nil || err.Error() != "source error: permission" {
		t.Fatalf("want permission error, got %v", err)
	}
}

func TestSessions(t *testing.T) {
	p := newProfile(t, "a")
	s := &SessionsSource{P: p, Interval: 15 * time.Second}
	defer func(f func(int) bool) { ProcAlive = f }(ProcAlive)

	ProcAlive = func(pid int) bool { return pid == 4242 }
	res, _, err := collect(t, s, t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := find(res.Samples, "agent_sessions", "state", "idle"); v != 1 {
		t.Fatalf("idle %v", v)
	}
	if v, ok := find(res.Samples, "agent_sessions", "state", "active"); !ok || v != 0 {
		t.Fatal("active must be emitted as 0")
	}
	noCanary(t, res.Samples, err)

	ProcAlive = func(int) bool { return false }
	res, _, _ = collect(t, s, t0, nil)
	if v, _ := find(res.Samples, "agent_sessions", "state", "idle"); v != 0 {
		t.Fatal("dead pid counted")
	}

	ProcAlive = func(int) bool { return true }
	write(t, filepath.Join(p.Dir, "sessions", "77.json"), []byte(`{"pid":77,"status":"busy"}`))
	write(t, filepath.Join(p.Dir, "sessions", "88.json"), []byte(`{nope`))
	res, _, _ = collect(t, s, t0, nil)
	if v, _ := find(res.Samples, "agent_sessions", "state", "active"); v != 1 {
		t.Fatal("busy must count as active")
	}
	if res.Errors["parse"] != 1 {
		t.Fatalf("parse errors %v", res.Errors)
	}
}

func TestSessionsDirMissing(t *testing.T) {
	p := newProfile(t, "a")
	os.RemoveAll(filepath.Join(p.Dir, "sessions"))
	res, _, err := collect(t, &SessionsSource{P: p, Interval: time.Minute}, t0, nil)
	if err != nil || len(res.Samples) != 3 {
		t.Fatalf("want three zero states, got %v %v", res.Samples, err)
	}
}

// A sessions dir or profile dir swapped for a link is refused, not followed (Qodo #3).
func TestSessionsAndAccountRefuseLinkedDirs(t *testing.T) {
	p := newProfile(t, "a")
	os.RemoveAll(filepath.Join(p.Dir, "sessions"))
	os.Symlink(t.TempDir(), filepath.Join(p.Dir, "sessions"))
	if _, _, err := collect(t, &SessionsSource{P: p, Interval: time.Minute}, t0, nil); err == nil {
		t.Fatal("linked sessions dir followed")
	}
	q := newProfile(t, "b")
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(q.Dir, link)
	q.Dir = link
	if _, _, err := collect(t, &AccountSource{P: q, Interval: time.Minute}, t0, nil); err == nil {
		t.Fatal("linked profile dir followed")
	}
}

// A configured ~/.claude that is a link to a dir with another name still finds the sibling ~/.claude.json
// (Qodo #13).
func TestClaudeJSONPathUsesConfiguredName(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "real-claude")
	os.MkdirAll(target, 0o755)
	if got := ClaudeJSONPath(target, filepath.Join(home, ".claude")); got != filepath.Join(home, ".claude.json") {
		t.Fatalf("got %s", got)
	}
	if got := ClaudeJSONPath(target, ""); got != filepath.Join(target, ".claude.json") {
		t.Fatalf("got %s", got)
	}
}
