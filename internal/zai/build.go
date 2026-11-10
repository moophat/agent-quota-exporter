package zai

import (
	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
)

// Build returns the source for one resolved zai profile and registers it with peers.
func Build(cfg *config.Config, r config.Resolved, host, version string, peers *Peers) []core.Source {
	if !r.Quota.Enabled("zai_usage") {
		return nil
	}
	s := &Source{
		Host:       host,
		Alias:      r.Alias,
		Account:    r.Account,
		Dir:        r.Dir,
		KeySource:  r.KeySource,
		Version:    version,
		Interval:   cfg.SourceInterval(SourceName, DefaultInterval),
		Backoff429: cfg.SourceBackoff(SourceName, DefaultBackoff429),
		StaleAfter: cfg.Defaults.StaleAfter.D(),
		Peers:      peers,
	}
	peers.Add(s)
	return []core.Source{s}
}
