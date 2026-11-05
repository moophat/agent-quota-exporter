package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Harness names this build has a plugin for.
const (
	HarnessClaude   = "claude-code"
	HarnessCodex    = "codex"
	HarnessOpencode = "opencode"
	HarnessRouter   = "9router"
	HarnessAgy      = "agy"
)

// Harnesses lists every harness with a plugin, so a typo in config fails loudly.
var Harnesses = []string{HarnessClaude, HarnessCodex, HarnessOpencode, HarnessRouter, HarnessAgy}

// quotaKeys are the opt-in quota sources a profile may enable, by harness. codex_rate_limits is reserved: v0.1
// accepted it, no plugin reads it yet, and Resolve warns that it is ignored.
var quotaKeys = map[string]string{"claude_oauth_usage": HarnessClaude, "claude_statusline_cache": HarnessClaude, "agy_usage": HarnessAgy, "codex_rate_limits": HarnessCodex}

// Floors are the vendor-safe minimums (specs/plan.md, "Scrape-time collection").
var Floors = map[string]struct{ MinInterval, Backoff429 time.Duration }{
	"claude-oauth-usage": {15 * time.Minute, 60 * time.Minute},
	"claude-transcripts": {30 * time.Second, 0},
	"claude-sessions":    {5 * time.Second, 0},
	"claude-account":     {30 * time.Second, 0},
	"codex-rollouts":     {30 * time.Second, 0},
	"opencode-db":        {30 * time.Second, 0},
	"router-9router":     {30 * time.Second, 0},
	"agy-usage":          {15 * time.Minute, 60 * time.Minute}, // backoff_429 is its backoff after any failed run
}

// ContractLabels are the label names the metric contract owns; static labels may not reuse them.
var ContractLabels = []string{"host", "harness", "profile", "account", "provider", "model", "window", "scope",
	"severity", "type", "outcome", "code", "plan", "state", "source", "reason", "version", "contract", "origin"}

// MaxLabels caps labels per series (contract, "Label vocabulary").
const MaxLabels = 12

// maxContractLabelsPerSeries is the widest contract label set (agent_quota_limit_info: 8).
const maxContractLabelsPerSeries = 8

var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Warnings collected during validation (non-fatal), for the caller to log once.
func (c *Config) Validate() error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if c.Version != 1 {
		add("version: must be 1, got %d", c.Version)
	}
	if c.Listen == "" {
		add("listen: must not be empty")
	}
	if c.StateDir == "" {
		add("state_dir: must not be empty")
	}
	switch c.Discovery.Mode {
	case "off", "add", "only":
	default:
		add("discovery.mode: %q is not one of off, add, only", c.Discovery.Mode)
	}
	switch c.Metrics.AccountLabel {
	case "alias", "email":
	default:
		add("metrics.account_label: %q is not one of alias, email", c.Metrics.AccountLabel)
	}
	if c.Metrics.MaxModelsPerProfile < 1 {
		add("metrics.max_models_per_profile: must be >= 1")
	}
	if c.Metrics.PerSession {
		add("metrics.per_session: not supported in this version")
	}
	if c.OTLP.Endpoint != "" {
		add("otlp.endpoint: not supported in this version")
	}
	if c.Defaults.StaleAfter.D() < 30*time.Minute {
		add("defaults.stale_after: must be >= 30m (contract staleness rule)")
	}
	if c.Defaults.UsageInterval.D() < Floors["claude-transcripts"].MinInterval {
		add("defaults.usage_interval: must be >= %s", Floors["claude-transcripts"].MinInterval)
	}

	for k := range c.Labels {
		if !labelName.MatchString(k) {
			add("labels.%s: invalid label name", k)
		}
		for _, cl := range ContractLabels {
			if k == cl {
				add("labels.%s: collides with a contract label", k)
			}
		}
	}
	if maxContractLabelsPerSeries+len(c.Labels) > MaxLabels {
		add("labels: %d static labels exceed the cap of %d labels per series", len(c.Labels), MaxLabels)
	}

	for name, s := range c.Sources {
		f, ok := Floors[name]
		if !ok {
			add("sources.%s: unknown source", name)
			continue
		}
		if s.IKnowThisCanLockMyAcc {
			continue
		}
		if s.MinInterval != 0 && s.MinInterval.D() < f.MinInterval {
			add("sources.%s.min_interval: %s is below the floor %s", name, s.MinInterval.D(), f.MinInterval)
		}
		if s.Backoff429 != 0 && s.Backoff429.D() < f.Backoff429 {
			add("sources.%s.backoff_429: %s is below the floor %s", name, s.Backoff429.D(), f.Backoff429)
		}
	}

	seen := map[string]bool{}
	for i, p := range c.Profiles {
		if p.Harness == "" {
			add("profiles[%d].harness: required", i)
		}
		if p.Alias == "" {
			add("profiles[%d].alias: required", i)
		}
		if contains(Harnesses, p.Harness) && p.Dir == "" {
			add("profiles[%d].dir: required for %s", i, p.Harness)
		}
		key := p.Harness + "/" + p.Alias
		if seen[key] {
			add("profiles[%d]: duplicate alias %q for harness %s", i, p.Alias, p.Harness)
		}
		seen[key] = true
		for q, value := range p.Quota {
			if q == "claude_statusline_cache" {
				path, ok := value.(string)
				if !ok || !filepath.IsAbs(path) {
					add("profiles[%d].quota.%s: must be an absolute file path", i, q)
				}
				if filepath.Clean(p.Dir) != "/root/.claude" {
					add("profiles[%d].quota.%s: only for /root/.claude", i, q)
				}
			} else if _, ok := value.(bool); !ok {
				add("profiles[%d].quota.%s: must be a boolean", i, q)
			}

			h, ok := quotaKeys[q]
			switch {
			case !ok:
				add("profiles[%d].quota.%s: unknown quota source", i, q)
			case h != p.Harness:
				add("profiles[%d].quota.%s: only for harness %s", i, q, h)
			}
		}
		if p.Harness == HarnessAgy && p.Quota.Enabled("agy_usage") && p.Account == "" {
			// one agy login sits on many machines; the operator names the account to read it from one host only
			add("profiles[%d].account: required for agy with quota.agy_usage", i)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("config invalid:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// SourceInterval returns the effective min interval for a source: config override, else def.
func (c *Config) SourceInterval(name string, def time.Duration) time.Duration {
	if s, ok := c.Sources[name]; ok && s.MinInterval != 0 {
		return s.MinInterval.D()
	}
	return def
}

// SourceBackoff returns the effective 429 backoff for a source: config override, else def.
func (c *Config) SourceBackoff(name string, def time.Duration) time.Duration {
	if s, ok := c.Sources[name]; ok && s.Backoff429 != 0 {
		return s.Backoff429.D()
	}
	return def
}

// secret-looking value prefixes: vendor key, JWT, auth scheme, PEM.
var secretPrefixes = []string{"sk-", "eyJ", "Bearer ", "bearer ", "-----BEGIN"}

// findSecretLike returns a description of the first scalar value that looks like an inline secret.
func findSecretLike(n *yaml.Node) string {
	if n.Kind == yaml.ScalarNode {
		v := strings.TrimSpace(n.Value)
		if strings.HasPrefix(v, "env:") || strings.HasPrefix(v, "file:") {
			return ""
		}
		for _, p := range secretPrefixes {
			if strings.HasPrefix(v, p) {
				return fmt.Sprintf("value at line %d", n.Line)
			}
		}
		return ""
	}
	for _, c := range n.Content {
		if r := findSecretLike(c); r != "" {
			return r
		}
	}
	return ""
}
