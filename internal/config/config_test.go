package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultsValid(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:9877" || c.Discovery.Mode != "add" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	if _, err := Load("../../examples/config.yaml"); err != nil {
		t.Fatal(err)
	}
}

// Each invalid config must fail loudly and name the offending key, because a silently ignored key
// (for example a lowered quota interval) is how an account gets rate-locked.
func TestValidationFailures(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"unknown key":      {"version: 1\nlisten2: x\n", "listen2"},
		"bad version":      {"version: 2\n", "version"},
		"duplicate alias":  {"version: 1\nprofiles:\n- {harness: claude-code, alias: a, dir: /x}\n- {harness: claude-code, alias: a, dir: /y}\n", "duplicate alias"},
		"inline secret":    {"version: 1\nlabels: {env: \"sk-abc\"}\n", "inline secret"},
		"inline jwt":       {"version: 1\nhost: eyJhbGciOi\n", "inline secret"},
		"below floor":      {"version: 1\nsources:\n  claude-oauth-usage: {min_interval: 1m}\n", "below the floor"},
		"backoff floor":    {"version: 1\nsources:\n  claude-oauth-usage: {backoff_429: 5m}\n", "backoff_429"},
		"unknown source":   {"version: 1\nsources:\n  nope: {min_interval: 1h}\n", "unknown source"},
		"label collision":  {"version: 1\nlabels: {host: x}\n", "collides"},
		"label name":       {"version: 1\nlabels: {\"1bad\": x}\n", "invalid label name"},
		"too many labels":  {"version: 1\nlabels: {a: 1, b: 1, c: 1, d: 1, e: 1}\n", "exceed the cap"},
		"otlp":             {"version: 1\notlp: {endpoint: http://x}\n", "otlp.endpoint"},
		"per session":      {"version: 1\nmetrics: {per_session: true}\n", "per_session"},
		"discovery mode":   {"version: 1\ndiscovery: {mode: all}\n", "discovery.mode"},
		"account label":    {"version: 1\nmetrics: {account_label: uuid}\n", "account_label"},
		"stale too short":  {"version: 1\ndefaults: {stale_after: 5m}\n", "stale_after"},
		"bad duration":     {"version: 1\ndefaults: {stale_after: soon}\n", "duration"},
		"claude needs dir": {"version: 1\nprofiles:\n- {harness: claude-code, alias: a}\n", "dir: required"},
		"unknown quota":    {"version: 1\nprofiles:\n- {harness: claude-code, alias: a, dir: /x, quota: {foo: true}}\n", "unknown quota"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeCfg(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestFloorOverrideNeedsExplicitFlag(t *testing.T) {
	body := "version: 1\nsources:\n  claude-oauth-usage: {min_interval: 1m, i_know_this_can_lock_my_account: true}\n"
	if _, err := Load(writeCfg(t, body)); err != nil {
		t.Fatal(err)
	}
}

func TestEnvAndFileReferencesAllowed(t *testing.T) {
	if _, err := Load(writeCfg(t, "version: 1\nlabels: {env: \"env:SK-NOT-A-SECRET\"}\n")); err != nil {
		t.Fatal(err)
	}
}

func mkdirs(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiscovery(t *testing.T) {
	r := t.TempDir()
	mkdirs(t, r, "root/.claude/projects", "home/a/.claude/sessions", "home/a/.claude-work/projects",
		"home/c/.claude", "home/d/.claude/projects", "elsewhere/x/projects")
	// b has only the sibling .claude.json file, not a config dir.
	mkdirs(t, r, "home/b")
	if err := os.WriteFile(filepath.Join(r, "home/b/.claude.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// e/.claude is a symlink to a/.claude: same real dir, must not be listed twice.
	mkdirs(t, r, "home/e")
	if err := os.Symlink(filepath.Join(r, "home/a/.claude"), filepath.Join(r, "home/e/.claude")); err != nil {
		t.Fatal(err)
	}
	c := Default()
	c.Discovery.Roots = []string{filepath.Join(r, "root"), filepath.Join(r, "home/*")}
	// d is configured explicitly: config wins, discovery must not add it again.
	c.Profiles = []Profile{{Harness: HarnessClaude, Alias: "dee", Dir: filepath.Join(r, "home/d/.claude")},
		{Harness: "codex", Alias: "cx", Dir: "/nowhere"}}
	env := func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return filepath.Join(r, "elsewhere/x")
		}
		return ""
	}
	got, warns := c.Resolve(env)
	var aliases []string
	for _, p := range got {
		aliases = append(aliases, p.Origin+":"+p.Alias)
	}
	want := "config:dee config:cx discovered:x discovered:a discovered:claude-work discovered:root"
	if strings.Join(aliases, " ") != want {
		t.Fatalf("got %q want %q", strings.Join(aliases, " "), want)
	}
	if len(warns) != 0 {
		t.Fatalf("codex has a plugin now; want no warnings, got %v", warns)
	}
}

// Codex homes and opencode data dirs are discovered beside Claude dirs. Aliases are per harness, so the same home
// yields "root" for each harness instead of "root-2"; a linked .local is not followed (as root, a user's link could
// expose another profile under their name).
func TestDiscoveryCodexAndOpencode(t *testing.T) {
	r := t.TempDir()
	mkdirs(t, r, "root/.claude/projects", "root/.codex/sessions/2026/11/04", "root/.local/share/opencode",
		"home/u/.codex", "home/v/.local/share/opencode", "target/share/opencode")
	for _, f := range []string{"root/.local/share/opencode/opencode.db", "target/share/opencode/opencode.db"} {
		if err := os.WriteFile(filepath.Join(r, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// home/w/.local is a link to a dir holding a real opencode.db: must not be discovered.
	mkdirs(t, r, "home/w")
	if err := os.Symlink(filepath.Join(r, "target"), filepath.Join(r, "home/w/.local")); err != nil {
		t.Fatal(err)
	}
	c := Default()
	c.Discovery.Roots = []string{filepath.Join(r, "root"), filepath.Join(r, "home/*")}
	got, _ := c.Resolve(func(string) string { return "" })
	var hs []string
	for _, p := range got {
		hs = append(hs, p.Harness+":"+p.Alias)
	}
	// u/.codex has no sessions/ and v's opencode dir has no opencode.db: neither is a profile.
	want := "claude-code:root codex:root opencode:root"
	if strings.Join(hs, " ") != want {
		t.Fatalf("got %q want %q", strings.Join(hs, " "), want)
	}
}

func TestValidateHarnessesAndQuotaKeys(t *testing.T) {
	for name, body := range map[string]string{
		"unknown harness":         "version: 1\nprofiles: [{harness: cursor, alias: a, dir: /x}]\n",
		"dir required":            "version: 1\nprofiles: [{harness: codex, alias: a}]\n",
		"quota key wrong harness": "version: 1\nprofiles: [{harness: codex, alias: a, dir: /x, quota: {claude_oauth_usage: true}}]\n",
		"unbuilt quota key":       "version: 1\nprofiles: [{harness: codex, alias: a, dir: /x, quota: {codex_rate_limits: true}}]\n",
	} {
		c := Default()
		if err := decode([]byte(body), c); err == nil {
			if err := c.Validate(); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
	}
	c := Default()
	ok := "version: 1\nprofiles: [{harness: agy, alias: a, dir: /x, quota: {agy_usage: true}}]\n"
	if err := decode([]byte(ok), c); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestDiscoveryAliasCollision(t *testing.T) {
	r := t.TempDir()
	mkdirs(t, r, "h1/alpha/.claude/projects", "h2/alpha/.claude/projects")
	c := Default()
	c.Discovery.Roots = []string{filepath.Join(r, "h1/*"), filepath.Join(r, "h2/*")}
	got, _ := c.Resolve(func(string) string { return "" })
	if len(got) != 2 || got[0].Alias != "alpha" || got[1].Alias != "alpha-2" {
		t.Fatalf("got %+v", got)
	}
}

func TestDiscoveryModes(t *testing.T) {
	r := t.TempDir()
	mkdirs(t, r, "h/u/.claude/projects", "cfg/projects")
	c := Default()
	c.Discovery.Roots = []string{filepath.Join(r, "h/*")}
	c.Profiles = []Profile{{Harness: HarnessClaude, Alias: "cfg", Dir: filepath.Join(r, "cfg")}}
	none := func(string) string { return "" }
	c.Discovery.Mode = "off"
	if got, _ := c.Resolve(none); len(got) != 1 || got[0].Alias != "cfg" {
		t.Fatalf("off: %+v", got)
	}
	c.Discovery.Mode = "only"
	if got, _ := c.Resolve(none); len(got) != 1 || got[0].Alias != "u" {
		t.Fatalf("only: %+v", got)
	}
}
