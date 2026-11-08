// Package app wires config, profiles, plugins and the core together. cmd/agent-quota-exporter is a thin shell
// around it so the end-to-end test drives the same wiring as production.
package app

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/agy"
	"github.com/moophat/agent-quota-exporter/internal/claude"
	"github.com/moophat/agent-quota-exporter/internal/codex"
	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/events"
	"github.com/moophat/agent-quota-exporter/internal/opencode"
	"github.com/moophat/agent-quota-exporter/internal/router"
	"github.com/moophat/agent-quota-exporter/internal/strata"
)

const (
	accountInterval  = 60 * time.Second
	sessionsInterval = 15 * time.Second
	quotaInterval    = 15 * time.Minute
	quotaBackoff     = 60 * time.Minute
)

// Built is everything main needs after wiring.
type Built struct {
	host        string
	Sources     []core.Source
	Profiles    map[[2]string]int // {harness, origin} → count
	Static      []core.Label
	Events      *events.Store
	IdentityKey []byte // private installation key; never exposed as a metric or event
}

// Build creates the plugin instances for the resolved profiles.
func Build(cfg *config.Config, profiles []config.Resolved, host, version string) (Built, error) {
	b := Built{Profiles: map[[2]string]int{}, host: host}
	var err error
	b.Events, b.IdentityKey, err = buildEvents(cfg, profiles)
	if err != nil {
		return b, err
	}
	cache := claude.NewQuotaCache()
	for _, lane := range cfg.Strata {
		b.Sources = append(b.Sources, &strata.Source{Host: host, Lane: lane.Name, BaseURL: lane.BaseURL, Interval: lane.PollInterval()})
	}
	for _, r := range profiles {
		var srcs []core.Source
		switch r.Harness {
		case config.HarnessCodex:
			srcs = codex.Build(cfg, r, host, version)
			for _, src := range srcs {
				if rollout, ok := src.(*codex.RolloutsSource); ok {
					rollout.Events = b.Events
				}
			}
		case config.HarnessOpencode:
			srcs = opencode.Build(cfg, r, host, b.Events)
		case config.HarnessRouter:
			srcs = router.Build(cfg, r, host)
			for _, src := range srcs {
				src.(*router.Source).SetEventStore(b.Events)
			}
		case config.HarnessAgy:
			srcs = agy.Build(cfg, r, host)
		}
		if r.Harness != config.HarnessClaude {
			if len(srcs) > 0 {
				b.Profiles[[2]string{r.Harness, r.Origin}]++
				b.Sources = append(b.Sources, srcs...)
			}
			continue
		}
		b.Profiles[[2]string{r.Harness, r.Origin}]++
		p := &claude.Profile{Host: host, Alias: r.Alias, Dir: r.Dir, ConfigDir: r.ConfigDir, AccountAlias: r.Account,
			EmailLabel: cfg.Metrics.AccountLabel == "email", MaxModels: cfg.Metrics.MaxModelsPerProfile,
			Events: b.Events, IdentityKey: b.IdentityKey}
		b.Sources = append(b.Sources,
			&claude.AccountSource{P: p, Interval: cfg.SourceInterval("claude-account", accountInterval)},
			&claude.SessionsSource{P: p, Interval: cfg.SourceInterval("claude-sessions", sessionsInterval)},
			&claude.TranscriptsSource{P: p, Interval: cfg.SourceInterval("claude-transcripts", cfg.Defaults.UsageInterval.D()), StateDir: cfg.StateDir, ByteBudget: cfg.Sources["claude-transcripts"].ByteBudget, Deadline: cfg.Sources["claude-transcripts"].Deadline.D()},
		)
		var statusline *claude.StatuslineCacheSource
		if filepath.Clean(r.Dir) == "/root/.claude" {
			path := r.Quota.StatuslinePath()
			if path == "" {
				path = os.Getenv("CK_USAGE_CACHE_PATH")
			}
			if path == "" {
				path = "/tmp/ck-usage-limits-cache.json"
			}
			statusline = &claude.StatuslineCacheSource{P: p, Path: path,
				Interval: cfg.Defaults.UsageInterval.D(), StaleAfter: cfg.Defaults.StaleAfter.D()}
			b.Sources = append(b.Sources, statusline)
		}
		if r.QuotaOptIn {
			b.Sources = append(b.Sources, &claude.OAuthUsageSource{P: p, Version: version, Cache: cache, Statusline: statusline,
				Interval:   cfg.SourceInterval("claude-oauth-usage", quotaInterval),
				Backoff429: cfg.SourceBackoff("claude-oauth-usage", quotaBackoff)})
		}
	}
	keys := make([]string, 0, len(cfg.Labels))
	for k := range cfg.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.Static = append(b.Static, core.Label{Name: k, Value: cfg.Labels[k]})
	}
	return b, nil
}

// IsAccount selects the account sources, which run first so every other plugin labels with the resolved account.
func IsAccount(s core.Source) bool { return s.Name() == "claude-account" }

// OnceFilter selects what --once runs: everything local, plus vendor sources only when asked (and then still
// subject to their persisted gates).
func OnceFilter(withQuota bool) func(core.Source) bool {
	return func(s core.Source) bool { return withQuota || !s.Network() }
}
