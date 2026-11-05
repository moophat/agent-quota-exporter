package app

import (
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/claude"
	"github.com/moophat/agent-quota-exporter/internal/config"
)

// Wiring can be checked without opening any live profile or credential file.
func TestStatuslineCacheWiring(t *testing.T) {
	for _, tc := range []struct {
		name, dir, env, path, want string
		oauth                      bool
	}{
		{name: "default", dir: "/root/.claude", want: "/tmp/ck-usage-limits-cache.json"},
		{name: "environment", dir: "/root/.claude", env: "/run/cache.json", want: "/run/cache.json"},
		{name: "config wins", dir: "/root/.claude", env: "/run/env.json", path: "/run/config.json", want: "/run/config.json", oauth: true},
		{name: "other home", dir: "/home/acct-a/.claude", env: "/run/cache.json"},
		{name: "other root profile", dir: "/root/.claude-work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CK_USAGE_CACHE_PATH", tc.env)
			cfg := config.Default()
			cfg.Defaults.UsageInterval = config.Duration(90 * time.Second)
			r := config.Resolved{Harness: config.HarnessClaude, Alias: "root", Dir: tc.dir, Account: "acct-a1", QuotaOptIn: tc.oauth, Quota: config.Quota{"claude_statusline_cache": tc.path}}
			built := Build(cfg, []config.Resolved{r}, "node-a", "test")
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
					t.Fatal("cache assigned to another login")
				}
				return
			}
			if local == nil || local.Path != tc.want || local.MinInterval() != 90*time.Second || local.StaleAfter != 5*time.Minute || local.Network() {
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
