package config

import (
	"fmt"
	"path/filepath"
	"time"
)

// Events configures optional private history. ByteBudget has no implicit default:
// enabling capture requires an explicit operator storage allocation.
type Events struct {
	Enabled          bool     `yaml:"enabled"`
	RootDir          string   `yaml:"root_dir"`
	ByteBudget       int64    `yaml:"byte_budget"`
	FreeSpaceReserve int64    `yaml:"free_space_reserve"`
	HotAge           Duration `yaml:"hot_age"`
	IdentityKeyFile  string   `yaml:"identity_key_file"`
	PrivacyProfile   string   `yaml:"privacy_profile"`
}

// ValidateEvents also supports callers that construct config directly.
func (c *Config) ValidateEvents() error {
	e := c.Events
	if e.PrivacyProfile != "community" && e.PrivacyProfile != "parity" {
		return fmt.Errorf("events.privacy_profile: must be community or parity")
	}
	if e.PrivacyProfile == "parity" && (!e.Enabled || c.Metrics.AccountLabel == "email") {
		return fmt.Errorf("events.privacy_profile: parity requires enabled capture and forbids metrics.account_label email")
	}
	if !e.Enabled {
		return nil
	}
	if !filepath.IsAbs(e.RootDir) {
		return fmt.Errorf("events.root_dir: must be an absolute directory path")
	}
	if e.ByteBudget <= 0 {
		return fmt.Errorf("events.byte_budget: must be positive and explicitly configured")
	}
	// Reserve one 4 MiB bounded chunk plus checkpoint/allocation space.
	if e.FreeSpaceReserve < 8<<20 {
		return fmt.Errorf("events.free_space_reserve: must be at least 8388608 bytes")
	}
	if e.HotAge.D() < 90*24*time.Hour {
		return fmt.Errorf("events.hot_age: must be at least 2160h (90 days)")
	}
	if e.IdentityKeyFile != "" && !filepath.IsAbs(e.IdentityKeyFile) {
		return fmt.Errorf("events.identity_key_file: must be an absolute file path")
	}
	return nil
}
