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
}

type Profile struct {
	Harness string          `yaml:"harness"`
	Alias   string          `yaml:"alias"`
	Dir     string          `yaml:"dir"`
	URL     string          `yaml:"url"`
	Account string          `yaml:"account"`
	Quota   map[string]bool `yaml:"quota"`
	Disable bool            `yaml:"disable"`
}

type Source struct {
	MinInterval           Duration `yaml:"min_interval"`
	Backoff429            Duration `yaml:"backoff_429"`
	IKnowThisCanLockMyAcc bool     `yaml:"i_know_this_can_lock_my_account"`
}

type OTLP struct {
	Endpoint string `yaml:"endpoint"`
}

// Default returns the configuration used when no file is given.
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
