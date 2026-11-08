package config

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
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

var legacyIdentityKeyWarning sync.Once

// ValidateEvents also supports callers that construct config directly.
func (c *Config) ValidateEvents() error {
	e := c.Events
	if e.IdentityKeyFile != "" {
		legacyIdentityKeyWarning.Do(func() { slog.Warn("events.identity_key_file is deprecated and ignored") })
	}
	if e.PrivacyProfile != "community" && e.PrivacyProfile != "parity" {
		return fmt.Errorf("events.privacy_profile: must be community or parity")
	}
	if e.PrivacyProfile == "parity" && !e.Enabled {
		return fmt.Errorf("events.privacy_profile: parity requires enabled capture")
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
	return nil
}
