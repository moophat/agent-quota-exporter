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
func Build(cfg *config.Config, r config.Resolved, host string) []core.Source {
	account := r.Account
	if account == "" {
		account = r.Alias // nothing is read from auth.json (design section 3)
	}
	return []core.Source{&RolloutsSource{
		Host:       host,
		Alias:      r.Alias,
		Account:    account,
		Dir:        r.Dir,
		MaxModels:  cfg.Metrics.MaxModelsPerProfile,
		StaleAfter: cfg.Defaults.StaleAfter.D(),
		Interval:   cfg.SourceInterval(SourceName, cfg.Defaults.UsageInterval.D()),
	}}
}

// labels returns the profile labels every sample carries, then extra.
func (s *RolloutsSource) labels(extra ...core.Label) []core.Label {
	ls := []core.Label{{Name: "host", Value: s.Host}, {Name: "harness", Value: Harness}, {Name: "profile", Value: s.Alias},
		{Name: "account", Value: s.Account}, {Name: "provider", Value: Provider}}
	return append(ls, extra...)
}
