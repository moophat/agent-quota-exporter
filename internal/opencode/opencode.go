// Package opencode reads an opencode database read-only (`opencode-db`): tokens, cost and errors per message.
package opencode

import (
	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
)

// Build returns the sources for one resolved opencode profile.
func Build(cfg *config.Config, r config.Resolved, host string) []core.Source {
	return nil
}
