// Package agy runs the agy client's own /usage command (`agy-usage`, opt-in) and parses its quota buckets.
package agy

import (
	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
)

// Build returns the sources for one resolved agy profile.
func Build(cfg *config.Config, r config.Resolved, host string) []core.Source {
	return nil
}
