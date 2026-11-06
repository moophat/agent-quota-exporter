package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/events"
)

func eventConfig(t *testing.T) *config.Config {
	t.Helper()
	c := config.Default()
	c.StateDir = filepath.Join(t.TempDir(), "state")
	c.Events.Enabled = true
	c.Events.RootDir = filepath.Join(t.TempDir(), "history")
	c.Events.ByteBudget = 1 << 20
	return c
}

func TestEventsDisabledDefaultUnchanged(t *testing.T) {
	c := eventConfig(t)
	c.Events.Enabled = false
	c.Events.IdentityKeyFile = "/missing/key"
	b, err := Build(c, nil, "node-a", "test")
	if err != nil || b.Events != nil || b.IdentityKey != nil || len(b.EventSamples()) != 0 {
		t.Fatalf("disabled store: %+v %v", b, err)
	}
	if _, err := os.Stat(c.Events.RootDir); !os.IsNotExist(err) {
		t.Fatalf("disabled store touched root: %v", err)
	}
}

func TestEventsBuildAndMetrics(t *testing.T) {
	c := eventConfig(t)
	b, err := Build(c, nil, "node-a", "test")
	if err != nil || b.Events == nil {
		t.Fatalf("build: %v", err)
	}
	raw := json.RawMessage(`{"schema_version":1,"kind":"usage","event_id":"e1","source":"claude-transcripts","parser_version":"v1","host":"node-a","profile":"work","harness":"claude-code","source_schema_version":null,"provider":null,"observed_at":{"original":null,"epoch_seconds":null,"quality":"unknown"},"collected_at":{"original":null,"epoch_seconds":1,"quality":"collection"},"locator":{"type":"probe","probe_id":"p1","part":"usage"},"quality":{"status":"observed","reasons":[],"missing_fields":[]},"data":{}}`)
	if err := b.Events.Append(events.Batch{Envelopes: []json.RawMessage{raw}}); err != nil {
		t.Fatal(err)
	}
	st, err := core.OpenState(c.StateDir, t0)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	clk := &clock{t: t0}
	sched := core.NewScheduler(nil, st, clk, slog.New(slog.NewTextHandler(io.Discard, nil)), c.Defaults.StaleAfter.D())
	sched.Tick(context.Background(), nil, true, true)
	exp := core.Exposition{Host: "node-a", Sched: sched, Clock: clk, ExtraSamples: b.EventSamples}
	var out bytes.Buffer
	if err := exp.Render(&out); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agent_event_store_size_bytes", "agent_event_store_available_bytes", "agent_event_store_pressure", "agent_event_store_write_success"} {
		if !strings.Contains(out.String(), name+"{") {
			t.Fatalf("missing %s: %s", name, out.String())
		}
	}
	c.Events.ByteBudget = 1
	b, err = Build(c, nil, "node-a", "test")
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"e1"`), []byte(`"e2"`), 1)
	if b.Events.Append(events.Batch{Envelopes: []json.RawMessage{raw}}) == nil {
		t.Fatal("budget failure expected")
	}
	for _, s := range b.EventSamples() {
		if s.Name == "agent_event_store_write_success" && s.Value != 0 {
			t.Fatal("failed append reported success")
		}
	}
	if err := os.RemoveAll(c.Events.RootDir); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range b.EventSamples() {
		if s.Name == "agent_event_store_write_success" && s.Value == 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("filesystem failure hid in-memory write outcome")
	}
}

func TestEventsStartupFailuresAndIdentityKey(t *testing.T) {
	for _, scenario := range []string{"budget", "unwritable", "missing-key", "required-key", "short-key", "public-key", "valid-key", "alias", "key-in-store"} {
		t.Run(scenario, func(t *testing.T) {
			c := eventConfig(t)
			profiles := []config.Resolved{{Account: "acct-a1"}}
			switch scenario {
			case "budget":
				c.Events.ByteBudget = -1
			case "unwritable":
				c.Events.RootDir = "/proc/aqe-event-store-test"
			case "missing-key":
				profiles[0].Account = ""
				c.Events.IdentityKeyFile = filepath.Join(t.TempDir(), "missing")
			case "required-key":
				profiles[0].Account = ""
			case "short-key", "public-key", "valid-key", "key-in-store":
				profiles[0].Account = ""
				c.Events.IdentityKeyFile = filepath.Join(t.TempDir(), "key")
				if scenario == "key-in-store" {
					c.Events.RootDir = filepath.Dir(c.Events.IdentityKeyFile)
				}
				key := bytes.Repeat([]byte{42}, 32)
				mode := os.FileMode(0600)
				if scenario == "short-key" {
					key = key[:31]
				}
				if scenario == "public-key" {
					mode = 0644
				}
				if err := os.WriteFile(c.Events.IdentityKeyFile, key, mode); err != nil {
					t.Fatal(err)
				}
			}
			b, err := Build(c, profiles, "node-a", "test")
			valid := scenario == "valid-key" || scenario == "alias"
			if valid != (err == nil) {
				t.Fatalf("build: %v", err)
			}
			if scenario == "valid-key" && len(b.IdentityKey) != 32 {
				t.Fatal("key not available to future adapters")
			}
		})
	}
}
