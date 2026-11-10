// Package config loads and validates the exporter configuration (specs/plan.md, "Config format").
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration written in YAML as a Go duration string ("60s", "15m").
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("line %d: duration must be a string such as \"15m\"", n.Line)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %v", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

type Config struct {
	Version   int               `yaml:"version"`
	Listen    string            `yaml:"listen"`
	Host      string            `yaml:"host"`
	StateDir  string            `yaml:"state_dir"`
	Labels    map[string]string `yaml:"labels"`
	Discovery Discovery         `yaml:"discovery"`
	Metrics   Metrics           `yaml:"metrics"`
	Defaults  Defaults          `yaml:"defaults"`
	Profiles  []Profile         `yaml:"profiles"`
	Sources   map[string]Source `yaml:"sources"`
	OTLP      OTLP              `yaml:"otlp"`
	Events    Events            `yaml:"events"`
	Strata    []Lane            `yaml:"strata"`
}

// Lane opts into one read-only Strata server. An empty list makes no requests.
type Lane struct {
	Name     string   `yaml:"name"`
	BaseURL  string   `yaml:"base_url"`
	Interval Duration `yaml:"interval"`
}

func (l Lane) PollInterval() time.Duration {
	if l.Interval == 0 {
		return 30 * time.Second
	}
	return l.Interval.D()
}

type Discovery struct {
	Mode  string   `yaml:"mode"`
	Roots []string `yaml:"roots"`
}

type Metrics struct {
	PerSession          bool   `yaml:"per_session"`
	MaxModelsPerProfile int    `yaml:"max_models_per_profile"`
	AccountLabel        string `yaml:"account_label"`
}

type Defaults struct {
	UsageInterval Duration `yaml:"usage_interval"`
	StaleAfter    Duration `yaml:"stale_after"`
	// StatuslineStaleAfter is accepted for configuration compatibility only;
	// all sources now use StaleAfter and core.FreshnessLimit.
	StatuslineStaleAfter Duration `yaml:"statusline_stale_after"`
}

type Profile struct {
	Harness string `yaml:"harness"`
	Alias   string `yaml:"alias"`
	Dir     string `yaml:"dir"`
	URL     string `yaml:"url"`
	Account string `yaml:"account"`
	// KeySource names the local harness login a zai profile reads its key from; zai only.
	KeySource string `yaml:"key_source"`
	Quota     Quota  `yaml:"quota"`
	Disable   bool   `yaml:"disable"`
}

// Quota holds boolean opt-ins and the optional statusline cache path.
type Quota map[string]any

func (q Quota) Enabled(key string) bool { v, _ := q[key].(bool); return v }
func (q Quota) StatuslinePath() string  { v, _ := q["claude_statusline_cache"].(string); return v }

type Source struct {
	ByteBudget            int64    `yaml:"byte_budget"`
	Deadline              Duration `yaml:"deadline"`
	MinInterval           Duration `yaml:"min_interval"`
	Backoff429            Duration `yaml:"backoff_429"`
	IKnowThisCanLockMyAcc bool     `yaml:"i_know_this_can_lock_my_account"`
}

type OTLP struct {
	Endpoint string `yaml:"endpoint"`
}

// Default returns the configuration used when no file is given.
// StatuslineStale returns the configured statusline floor, else 5m.
func (d Defaults) StatuslineStale() time.Duration {
	if v := d.StatuslineStaleAfter.D(); v > 0 {
		return v
	}
	return 5 * time.Minute
}

func Default() *Config {
	return &Config{
		Version:  1,
		Listen:   "127.0.0.1:9877",
		StateDir: "/var/lib/agent-quota-exporter",
		Discovery: Discovery{
			Mode:  "add",
			Roots: []string{"/root", "/home/*"},
		},
		Metrics: Metrics{MaxModelsPerProfile: 20, AccountLabel: "alias"},
		Events:  Events{FreeSpaceReserve: 8 << 20, HotAge: Duration(90 * 24 * time.Hour), PrivacyProfile: "community"},
		Defaults: Defaults{
			UsageInterval: Duration(60 * time.Second),
			StaleAfter:    Duration(45 * time.Minute),
		},
	}
}

// Load reads path (empty = defaults only), applies defaults and validates.
func Load(path string) (*Config, error) {
	c := Default()
	if path == "" {
		return c, c.Validate()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := decode(b, c); err != nil {
		return nil, err
	}
	return c, c.Validate()
}

func decode(b []byte, c *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("config: %w", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(b, &node); err == nil {
		if p := findSecretLike(&node); p != "" {
			return fmt.Errorf("config: %s looks like an inline secret; reference it with env:NAME or file:/path", p)
		}
	}
	return nil
}
