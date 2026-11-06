package config

import (
	"strings"
	"testing"
)

func TestEventsConfigDefaultsAndValidation(t *testing.T) {
	c := Default()
	if err := decode([]byte("version: 1\nevents:\n  enabled: true\n  root_dir: /tmp/history\n  byte_budget: 1024\n"), c); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Events.FreeSpaceReserve != 8<<20 || c.Events.HotAge.D().Hours() != 2160 || c.Events.PrivacyProfile != "community" {
		t.Fatalf("defaults: %+v", c.Events)
	}
	for _, tc := range []struct{ field, yaml string }{
		{"byte_budget", "byte_budget: 0"},
		{"byte_budget", "byte_budget: -1"},
		{"root_dir", "root_dir: relative"},
		{"free_space_reserve", "free_space_reserve: 1"},
		{"hot_age", "hot_age: 24h"},
		{"privacy_profile", "privacy_profile: typo"},
		{"identity_key_file", "identity_key_file: relative"},
	} {
		t.Run(tc.yaml, func(t *testing.T) {
			copy := *c
			if err := decode([]byte("events:\n  "+tc.yaml+"\n"), &copy); err != nil {
				t.Fatal(err)
			}
			if err := copy.Validate(); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("validation: %v", err)
			}
		})
	}
	c.Events.PrivacyProfile = "parity"
	c.Metrics.AccountLabel = "email"
	if c.Validate() == nil {
		t.Fatal("parity accepted email metrics")
	}
	c.Metrics.AccountLabel = "alias"
	c.Events.Enabled = false
	if c.Validate() == nil {
		t.Fatal("parity accepted disabled store")
	}
}
