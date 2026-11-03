package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

const (
	tokenCanary   = "CANARY-ACCESS-TOKEN"
	contentCanary = "CANARY-CONTENT"
)

// t0 is the fixture epoch: 2026-01-01T00:00:00Z.
var t0 = time.Unix(1767225600, 0).UTC()

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "claude", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// newProfile builds <tmp>/home/<alias>/.claude with the sibling ~/.claude.json layout of a default install.
func newProfile(t *testing.T, alias string) *Profile {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home", alias)
	dir := filepath.Join(home, ".claude")
	write(t, filepath.Join(home, ".claude.json"), fixture(t, "dot-claude.json"))
	write(t, filepath.Join(dir, ".credentials.json"), fixture(t, "dot-credentials.json"))
	write(t, filepath.Join(dir, "sessions", "4242.json"), fixture(t, "sessions/4242.json"))
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Profile{Host: "node-a", Alias: alias, Dir: dir, MaxModels: 20}
}

func collect(t *testing.T, s core.Source, now time.Time, st json.RawMessage) (core.Result, json.RawMessage, error) {
	t.Helper()
	return s.Collect(context.Background(), now, st)
}

func find(ss []core.Sample, name string, kv ...string) (float64, bool) {
next:
	for _, s := range ss {
		if s.Name != name {
			continue
		}
		for i := 0; i+1 < len(kv); i += 2 {
			ok := false
			for _, l := range s.Labels {
				if l.Name == kv[i] && l.Value == kv[i+1] {
					ok = true
				}
			}
			if !ok {
				continue next
			}
		}
		return s.Value, true
	}
	return 0, false
}

// noCanary fails if any sample label or the error text carries a canary.
func noCanary(t *testing.T, ss []core.Sample, err error, extra ...string) {
	t.Helper()
	var b strings.Builder
	for _, s := range ss {
		for _, l := range s.Labels {
			b.WriteString(l.Value + " ")
		}
	}
	if err != nil {
		b.WriteString(err.Error())
	}
	for _, e := range extra {
		b.WriteString(e)
	}
	if strings.Contains(b.String(), "CANARY") {
		t.Fatalf("canary leaked: %s", b.String())
	}
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }
