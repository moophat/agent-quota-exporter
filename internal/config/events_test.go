package config

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
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
	if err := c.Validate(); err != nil {
		t.Fatalf("parity email metrics: %v", err)
	}
	c.Metrics.AccountLabel = "alias"
	c.Events.Enabled = false
	if c.Validate() == nil {
		t.Fatal("parity accepted disabled store")
	}
}

func TestLegacyIdentityKeyIgnored(t *testing.T) {
	legacyIdentityKeyWarning = sync.Once{}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	for _, path := range []string{"relative", "/nonexistent/legacy-key"} {
		c := Default()
		if err := decode([]byte("version: 1\nevents:\n  enabled: true\n  root_dir: /tmp/history\n  byte_budget: 1024\n  identity_key_file: "+path+"\n"), c); err != nil {
			t.Fatal(err)
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("legacy key %q: %v", path, err)
		}
	}
	if strings.Count(logs.String(), "level=WARN") != 1 || strings.Contains(logs.String(), "/nonexistent/legacy-key") || strings.Contains(logs.String(), "relative") {
		t.Fatalf("legacy warning must occur once without supplied paths: %s", logs.String())
	}
}

func TestTranscriptByteBudgetCoversOneLine(t *testing.T) {
	for budget, ok := range map[string]bool{"0": true, "67108864": true, "1048576": false} {
		c := Default()
		if err := decode([]byte("version: 1\nsources:\n  claude-transcripts:\n    byte_budget: "+budget+"\n"), c); err != nil {
			t.Fatal(err)
		}
		err := c.Validate()
		if (err == nil) != ok || (err != nil && !strings.Contains(err.Error(), "64 MiB")) {
			t.Fatalf("byte_budget %s: %v", budget, err)
		}
	}
}
