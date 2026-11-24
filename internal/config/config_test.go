package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		"unknown key":           {"version: 1\nlisten2: x\n", "listen2"},
		"bad version":           {"version: 2\n", "version"},
		"duplicate alias":       {"version: 1\nprofiles:\n- {harness: claude-code, alias: a, dir: /x}\n- {harness: claude-code, alias: a, dir: /y}\n", "duplicate alias"},
		"inline secret":         {"version: 1\nlabels: {env: \"sk-abc\"}\n", "inline secret"},
		"inline jwt":            {"version: 1\nhost: eyJhbGciOi\n", "inline secret"},
		"below floor":           {"version: 1\nsources:\n  claude-oauth-usage: {min_interval: 1m}\n", "below the floor"},
		"backoff floor":         {"version: 1\nsources:\n  claude-oauth-usage: {backoff_429: 5m}\n", "backoff_429"},
		"agy below floor":       {"version: 1\nsources:\n  agy-usage: {min_interval: 90s}\n", "below the floor"},
		"codex below floor":     {"version: 1\nsources:\n  codex-usage: {min_interval: 30s}\n", "below the floor"},
		"codex 429 floor":       {"version: 1\nsources:\n  codex-usage: {backoff_429: 1m}\n", "backoff_429"},
		"codex_usage on claude": {"version: 1\nprofiles:\n- {harness: claude-code, alias: a, dir: /x, quota: {codex_usage: true}}\n", "only for harness codex"},
		"codex_usage not bool":  {"version: 1\nprofiles:\n- {harness: codex, alias: a, dir: /x, quota: {codex_usage: 3}}\n", "must be a boolean"},
		"unknown source":        {"version: 1\nsources:\n  nope: {min_interval: 1h}\n", "unknown source"},
		"label collision":       {"version: 1\nlabels: {host: x}\n", "collides"},
		"label name":            {"version: 1\nlabels: {\"1bad\": x}\n", "invalid label name"},
		"too many labels":       {"version: 1\nlabels: {a: 1, b: 1, c: 1, d: 1, e: 1}\n", "exceed the cap"},
		"otlp":                  {"version: 1\notlp: {endpoint: http://x}\n", "otlp.endpoint"},
		"discovery mode":        {"version: 1\ndiscovery: {mode: all}\n", "discovery.mode"},
		"account label":         {"version: 1\nmetrics: {account_label: uuid}\n", "account_label"},
		"stale nonpositive":     {"version: 1\ndefaults: {stale_after: 0s}\n", "stale_after"},
		"bad duration":          {"version: 1\ndefaults: {stale_after: soon}\n", "duration"},
		"claude needs dir":      {"version: 1\nprofiles:\n- {harness: claude-code, alias: a}\n", "dir: required"},
		"unknown quota":         {"version: 1\nprofiles:\n- {harness: claude-code, alias: a, dir: /x, quota: {foo: true}}\n", "unknown quota"},
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

// agy reads through the local client at 0 tokens, so a deployment may poll it every 2 minutes; codex-usage has
// its own, tighter floors and a quota key of its own.
func TestFreshQuotaSettingsAccepted(t *testing.T) {
	body := "version: 1\nsources:\n  agy-usage: {min_interval: 2m}\n  codex-usage: {min_interval: 1m, backoff_429: 5m}\n" +
		"profiles:\n- {harness: codex, alias: a, dir: /x, quota: {codex_usage: true}}\n"
	c, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.SourceInterval("agy-usage", 30*time.Minute); got != 2*time.Minute {
		t.Errorf("agy interval %s", got)
	}
	if !c.Profiles[0].Quota.Enabled("codex_usage") {
		t.Error("codex_usage not read")
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
		"dir required":              "version: 1\nprofiles: [{harness: codex, alias: a}]\n",
		"quota key wrong harness":   "version: 1\nprofiles: [{harness: codex, alias: a, dir: /x, quota: {claude_oauth_usage: true}}]\n",
		"agy usage without account": "version: 1\nprofiles: [{harness: agy, alias: a, dir: /x, quota: {agy_usage: true}}]\n",
	} {
		c := Default()
		if err := decode([]byte(body), c); err == nil {
			if err := c.Validate(); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
	}
	c := Default()
	ok := "version: 1\nprofiles: [{harness: agy, alias: a, dir: /x, account: agy-a1, quota: {agy_usage: true}}]\n"
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

// v0.1 configs keep loading: an unknown harness is warned about and skipped, and the reserved codex_rate_limits key
// is accepted with a warning that nothing reads it yet.
func TestV01ConfigsStillLoad(t *testing.T) {
	c := Default()
	body := "version: 1\ndiscovery: {mode: \"off\"}\nprofiles:\n" +
		"  - {harness: cursor, alias: cu, dir: /x}\n" +
		"  - {harness: codex, alias: cx, dir: /y, quota: {codex_rate_limits: true}}\n"
	if err := decode([]byte(body), c); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("a v0.1 config was rejected: %v", err)
	}
	got, warns := c.Resolve(func(string) string { return "" })
	if len(got) != 1 || got[0].Harness != HarnessCodex {
		t.Fatalf("want only the codex profile resolved, got %+v", got)
	}
	if len(warns) != 2 || !strings.Contains(warns[0], "cursor") || !strings.Contains(warns[1], "codex_rate_limits") {
		t.Fatalf("want a skip warning and a reserved-key warning, got %v", warns)
	}
}

func TestStatuslineCacheConfig(t *testing.T) {
	body := "version: 1\ndiscovery: {mode: off}\nprofiles:\n- harness: claude-code\n  alias: root\n  dir: /root/.claude\n  quota: {claude_statusline_cache: /run/claude-usage/cache.json, claude_oauth_usage: true}\n"
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatal(err)
	}
	profiles, _ := cfg.Resolve(func(string) string { return "" })
	if len(profiles) != 1 || profiles[0].Quota.StatuslinePath() != "/run/claude-usage/cache.json" || !profiles[0].QuotaOptIn {
		t.Fatalf("cache config lost: %+v", profiles)
	}
	if _, err := Load(writeCfg(t, strings.Replace(body, "/root/.claude", "/home/acct-a/.claude", 1))); err != nil {
		t.Fatalf("a cache path must be accepted for any Claude profile dir: %v", err)
	}
	for _, bad := range []string{
		strings.Replace(body, "/run/claude-usage/cache.json", "relative.json", 1),
		strings.Replace(body, "/run/claude-usage/cache.json", "true", 1),
		strings.Replace(body, "claude_oauth_usage: true", "claude_oauth_usage: /run/file", 1),
		strings.Replace(body, "harness: claude-code", "harness: codex", 1),
	} {
		if _, err := Load(writeCfg(t, bad)); err == nil {
			t.Fatalf("invalid cache config accepted: %s", bad)
		}
	}
}

// The statusline setting is a configured floor; core applies the shared freshness formula.
func TestStatuslineStaleAfterDefaultsShortAndSeparate(t *testing.T) {
	c, err := Load(writeCfg(t, "version: 1\ndefaults: {usage_interval: 60s, stale_after: 45m}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Defaults.StatuslineStale(); got != 5*time.Minute {
		t.Fatalf("statusline window default = %s, want 5m", got)
	}
	if got := c.Defaults.StaleAfter.D(); got != 45*time.Minute {
		t.Fatalf("stale_after = %s, want 45m unchanged", got)
	}
	if _, err := Load(writeCfg(t, "version: 1\ndefaults: {usage_interval: 60s, statusline_stale_after: 90s}\n")); err != nil {
		t.Fatal(err)
	}
	// An existing slow-poll config keeps the same configured floor.
	slow, err := Load(writeCfg(t, "version: 1\ndefaults: {usage_interval: 10m, stale_after: 45m}\n"))
	if err != nil {
		t.Fatalf("slow-poll config rejected: %v", err)
	}
	if got := slow.Defaults.StatuslineStale(); got != 5*time.Minute {
		t.Fatalf("configured floor = %s, want 5m", got)
	}
}
func TestFiveMinuteStaleAfterAccepted(t *testing.T) {
	c, err := Load(writeCfg(t, "version: 1\ndefaults: {stale_after: 5m}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Defaults.StaleAfter.D() != 5*time.Minute {
		t.Fatal("stale_after not preserved")
	}
}
