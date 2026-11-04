// Package codex reads Codex CLI rollouts (`codex-rollouts`): quota, credits and tokens the client already wrote.
package codex

import (
	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
)

// Build returns the sources for one resolved codex profile.
func Build(cfg *config.Config, r config.Resolved, host string) []core.Source {
	return nil
}
