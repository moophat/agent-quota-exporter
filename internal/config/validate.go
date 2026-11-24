package config

import (
	"fmt"
	"net"
	"net/url"
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
	HarnessZAI      = "zai"
)

// ZAIKeySources are the harness logins a zai profile may read the key from. zcode is deliberately absent.
var ZAIKeySources = []string{"openclaw", "opencode", "zai-helper"}

// Harnesses lists every harness with a plugin, so a typo in config fails loudly.
var Harnesses = []string{HarnessClaude, HarnessCodex, HarnessOpencode, HarnessRouter, HarnessAgy, HarnessZAI}

// quotaKeys are the opt-in quota sources a profile may enable, by harness. codex_rate_limits is reserved: v0.1
// accepted it, no plugin reads it yet, and Resolve warns that it is ignored.
var quotaKeys = map[string]string{"claude_oauth_usage": HarnessClaude, "claude_statusline_cache": HarnessClaude, "agy_usage": HarnessAgy,
	"codex_rate_limits": HarnessCodex, "codex_usage": HarnessCodex, "zai_usage": HarnessZAI}

// Floors are the vendor-safe minimums (specs/plan.md, "Scrape-time collection").
var Floors = map[string]struct{ MinInterval, Backoff429 time.Duration }{
	"claude-oauth-usage": {15 * time.Minute, 60 * time.Minute},
	"claude-transcripts": {30 * time.Second, 0},
	"claude-sessions":    {5 * time.Second, 0},
	"claude-account":     {30 * time.Second, 0},
	"codex-rollouts":     {30 * time.Second, 0},
	"opencode-db":        {30 * time.Second, 0},
	"opencode-http":      {5 * time.Second, 0},
	"job-locks":          {5 * time.Second, 0},
	"router-9router":     {30 * time.Second, 0},
	// agy-usage reads through the local client at 0 tokens, so its interval floor is low; backoff_429 is its
	// backoff after any failed run.
	"agy-usage": {2 * time.Minute, 60 * time.Minute},
	// codex-usage calls the ChatGPT backend with the CLI's own access token, never refreshing it.
	"codex-usage": {time.Minute, 5 * time.Minute},
	// zai-usage calls an undocumented, community-used z.ai endpoint with a key a local harness already holds.
	"zai-usage": {5 * time.Minute, 10 * time.Minute},
}

// ContractLabels are the label names the metric contract owns; static labels may not reuse them.
var ContractLabels = []string{"host", "harness", "profile", "account", "provider", "model", "window", "scope",
	"severity", "type", "outcome", "code", "plan", "state", "source", "reason", "version", "contract", "origin",
	"session", "tmux_session", "entrypoint"}

// LaneLabels are owned by the lane series; static labels may reuse them only while no lane source is configured.
var LaneLabels = []string{"lane", "images", "context"}

// PerSessionLabels are owned by agent_session_info; static labels may reuse them only while metrics.per_session is
// off, so a config that already used one keeps starting until it opts in to the per-session series.
var PerSessionLabels = []string{"pid"}

// MaxLabels caps labels per series (contract, "Label vocabulary").
const MaxLabels = 12

// maxContractLabelsPerSeries is the widest contract label set (agent_quota_limit_info and agent_session_info: 8).
const maxContractLabelsPerSeries = 8

var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Warnings collected during validation (non-fatal), for the caller to log once.
func (c *Config) Validate() error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	lanes := map[string]bool{}
	for i, l := range c.Strata {
		if l.Name == "" || len(l.Name) > 128 || lanes[l.Name] {
			add("strata[%d].name: required, unique and at most 128 bytes", i)
		}
		lanes[l.Name] = true
		u, err := url.Parse(l.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || strings.ContainsAny(l.BaseURL, "?#") {
			add("strata[%d].base_url: must be an HTTP(S) URL without credentials, query or fragment", i)
		}
		if l.PollInterval() < 5*time.Second {
			add("strata[%d].interval: must be >= 5s", i)
		}
	}
	if err := c.ValidateEvents(); err != nil {
		add("%s", err)
	}

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
	if c.Metrics.MaxSessionsPerProfile < 1 || c.Metrics.MaxSessionsPerProfile > 500 {
		add("metrics.max_sessions_per_profile: must be between 1 and 500")
	}
	for k, v := range map[string]string{"jobs.lock_dir": c.Jobs.LockDir, "jobs.call_log": c.Jobs.CallLog} {
		if v != "" && !filepath.IsAbs(v) {
			add("%s: must be an absolute path", k)
		}
	}
	if c.OTLP.Endpoint != "" {
		add("otlp.endpoint: not supported in this version")
	}
	if v := c.Defaults.StatuslineStaleAfter.D(); v < 0 {
		add("defaults.statusline_stale_after: must be positive when set")
	}
	if c.Defaults.StaleAfter.D() <= 0 {
		add("defaults.stale_after: must be positive")
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
		if c.Metrics.PerSession {
			for _, pl := range PerSessionLabels {
				if k == pl {
					add("labels.%s: collides with the %s label of agent_session_info (metrics.per_session is on); rename this static label or turn per_session off", k, pl)
				}
			}
		}
		if len(c.Strata) > 0 {
			for _, ll := range LaneLabels {
				if k == ll {
					add("labels.%s: collides with a lane label (strata is configured)", k)
				}
			}
		}
	}
	if maxContractLabelsPerSeries+len(c.Labels) > MaxLabels {
		add("labels: %d static labels exceed the cap of %d labels per series", len(c.Labels), MaxLabels)
	}

	for name, s := range c.Sources {
		if s.ByteBudget < 0 || s.Deadline < 0 {
			add("sources.%s: byte_budget and deadline must be nonnegative", name)
		}
		// A budget below the transcript line limit could never finish a valid long line and would stall that file.
		if s.ByteBudget > 0 && s.ByteBudget < 64<<20 {
			add("sources.%s: byte_budget must be 0 (default) or at least 64 MiB, the transcript line limit", name)
		}
		if name != "claude-transcripts" && (s.ByteBudget != 0 || s.Deadline != 0) {
			add("sources.%s: byte_budget and deadline are only supported for claude-transcripts", name)
		}
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
		c.validateZAI(i, p, add)
		c.validateOpencodeURL(i, p, add)
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

// validateZAI checks the zai-only profile fields; key_source is rejected on every other harness.
func (c *Config) validateZAI(i int, p Profile, add func(string, ...any)) {
	if p.Harness != HarnessZAI {
		if p.KeySource != "" {
			add("profiles[%d].key_source: only for harness %s", i, HarnessZAI)
		}
		return
	}
	switch {
	case p.KeySource == "":
		add("profiles[%d].key_source: required (one of %s)", i, strings.Join(ZAIKeySources, ", "))
	case p.KeySource == "zcode":
		add("profiles[%d].key_source: zcode is unsupported (its login is not a plain key)", i)
	case !contains(ZAIKeySources, p.KeySource):
		add("profiles[%d].key_source: %q is not one of %s", i, p.KeySource, strings.Join(ZAIKeySources, ", "))
	}
	if p.Account == "" {
		add("profiles[%d].account: required for zai", i)
	}
	raw, present := p.Quota["zai_usage"]
	if v, isBool := raw.(bool); (!present || isBool) && !v {
		add("profiles[%d].quota.zai_usage: must be true (the opt-in)", i)
	}
}

// validateOpencodeURL checks the opencode-http address: an HTTP(S) URL without credentials, query or fragment, on
// loopback unless the profile sets allow_remote. allow_remote is rejected on every other harness.
func (c *Config) validateOpencodeURL(i int, p Profile, add func(string, ...any)) {
	if p.Harness != HarnessOpencode {
		if p.AllowRemote {
			add("profiles[%d].allow_remote: only for harness %s", i, HarnessOpencode)
		}
		return
	}
	if p.URL == "" {
		if p.AllowRemote {
			add("profiles[%d].allow_remote: needs url", i)
		}
		return
	}
	u, err := url.Parse(p.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || strings.ContainsAny(p.URL, "?#") {
		add("profiles[%d].url: must be an HTTP(S) URL without credentials, query or fragment", i)
		return
	}
	if !p.AllowRemote && !isLoopbackHost(u.Hostname()) {
		add("profiles[%d].url: must be on loopback unless allow_remote is true", i)
	}
}

// LoopbackURL reports whether a URL's host is loopback (an unparsable URL is not).
func LoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && isLoopbackHost(u.Hostname())
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
