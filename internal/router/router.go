// Package router reads a 9router database read-only (`router-9router`): requests, tokens and connection health.
package router

import (
	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
)

// Build returns the sources for one resolved 9router profile.
func Build(cfg *config.Config, r config.Resolved, host string) []core.Source {
	return nil
}
