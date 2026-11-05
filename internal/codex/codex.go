// Package codex reads Codex CLI rollouts (`codex-rollouts`): quota, credits and tokens the client already wrote.
package codex

import (
	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
)

const (
	Harness    = "codex"
	Provider   = "openai"
	SourceName = "codex-rollouts"
)

// Build returns the sources for one resolved codex profile.
func Build(cfg *config.Config, r config.Resolved, host, version string) []core.Source {
	account := r.Account
	if account == "" {
		account = r.Alias // nothing is read from auth.json (design section 3)
	}
	usage := r.Quota.Enabled("codex_usage")
	srcs := []core.Source{&RolloutsSource{
		Host:       host,
		Alias:      r.Alias,
		Account:    account,
		Dir:        r.Dir,
		MaxModels:  cfg.Metrics.MaxModelsPerProfile,
		StaleAfter: cfg.Defaults.StaleAfter.D(),
		Interval:   cfg.SourceInterval(SourceName, cfg.Defaults.UsageInterval.D()),
		// With codex-usage on, it owns the quota series: rollouts keeps only its token counters, so no series
		// is emitted twice.
		SkipQuota: usage,
	}}
	if usage {
		srcs = append(srcs, &UsageSource{
			Host:       host,
			Alias:      r.Alias,
			Account:    account,
			Dir:        r.Dir,
			StaleAfter: cfg.Defaults.StaleAfter.D(),
			Interval:   cfg.SourceInterval(UsageSourceName, defaultUsageInterval),
			Backoff429: cfg.SourceBackoff(UsageSourceName, defaultUsageBackoff),
			Version:    version,
		})
	}
	return srcs
}

// labels returns the profile labels every sample carries, then extra.
func (s *RolloutsSource) labels(extra ...core.Label) []core.Label {
	return ident{s.Host, s.Alias, s.Account}.labels(extra...)
}
