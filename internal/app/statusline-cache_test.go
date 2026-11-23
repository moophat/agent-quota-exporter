package app

import (
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/claude"
	"github.com/moophat/agent-quota-exporter/internal/config"
)

// Wiring can be checked without opening any live profile or credential file.
// The source exists only when a profile names its cache file, for any profile dir.
func TestStatuslineCacheWiring(t *testing.T) {
	for _, tc := range []struct {
		name, dir, path, want string
		oauth                 bool
	}{
		{name: "off by default", dir: "/root/.claude"},
		{name: "off by default with oauth", dir: "/home/acct-a/.claude", oauth: true},
		{name: "configured", dir: "/home/acct-a/.claude", path: "/run/claude-usage/cache.json", want: "/run/claude-usage/cache.json"},
		{name: "configured with oauth", dir: "/root/.claude-work", path: "/run/config.json", want: "/run/config.json", oauth: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Defaults.UsageInterval = config.Duration(90 * time.Second)
			r := config.Resolved{Harness: config.HarnessClaude, Alias: "root", Dir: tc.dir, Account: "acct-a1", QuotaOptIn: tc.oauth, Quota: config.Quota{"claude_statusline_cache": tc.path}}
			built, err := Build(cfg, []config.Resolved{r}, "node-a", "test")
			if err != nil {
				t.Fatal(err)
			}
			var local *claude.StatuslineCacheSource
			var oauth *claude.OAuthUsageSource
			for _, src := range built.Sources {
				switch s := src.(type) {
				case *claude.StatuslineCacheSource:
					local = s
				case *claude.OAuthUsageSource:
					oauth = s
				}
			}
			if tc.want == "" {
				if local != nil {
					t.Fatal("cache source enabled without a configured path")
				}
				if tc.oauth && (oauth == nil || oauth.Statusline != nil) {
					t.Fatal("OAuth must run alone when no cache is configured")
				}
				return
			}
			if local == nil || local.Path != tc.want || local.MinInterval() != 90*time.Second || local.StaleAfter != 45*time.Minute || local.Network() {
				t.Fatalf("wrong local source: %+v", local)
			}
			if tc.oauth && (oauth == nil || oauth.Statusline != local || oauth.P != local.P || oauth.MinInterval() != 15*time.Minute) {
				t.Fatal("fallback must share the same profile and retain its floor")
			}
			if !tc.oauth && oauth != nil {
				t.Fatal("OAuth enabled implicitly")
			}
		})
	}
}
