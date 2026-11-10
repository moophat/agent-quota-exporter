package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/zai"
)

type zaiRT struct {
	mu    sync.Mutex
	calls int
	body  []byte
}

func (r *zaiRT) RoundTrip(*http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(r.body)), Header: http.Header{}}, nil
}

func (r *zaiRT) count() int { r.mu.Lock(); defer r.mu.Unlock(); return r.calls }

func zaiWorld(t *testing.T) (*config.Config, []config.Resolved, *zaiRT) {
	t.Helper()
	root := t.TempDir()
	const k = "CANARYZAI0000000000000000.CANARYZAI00000000"
	claw := filepath.Join(root, "openclaw")
	helper := filepath.Join(root, "chelper")
	for d, f := range map[string][2]string{
		claw:   {"openclaw.json", `{"models":{"providers":{"zai":{"apiKey":"` + k + `"}}}}`},
		helper: {"config.yaml", "api_key: ZZZZZZZZZZZZZZZZZZZZ\n"},
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, f[0]), []byte(f[1]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "zai", "quota-limit-used.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "c.yaml")
	yaml := "version: 1\nhost: node-a\nstate_dir: " + filepath.Join(root, "state") + "\ndiscovery: {mode: off}\nprofiles:\n" +
		"- {harness: zai, alias: zai-claw, account: zai-main, key_source: openclaw, dir: " + claw + ", quota: {zai_usage: true}}\n" +
		"- {harness: zai, alias: zai-helper, account: zai-second, key_source: zai-helper, dir: " + helper + ", quota: {zai_usage: true}}\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	profiles, _ := cfg.Resolve(func(string) string { return "" })
	return cfg, profiles, &zaiRT{body: body}
}

func TestZAIWiringThroughRender(t *testing.T) {
	cfg, profiles, rt := zaiWorld(t)
	b, err := Build(cfg, profiles, cfg.Host, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Sources) != 2 {
		t.Fatalf("%d sources", len(b.Sources))
	}
	for _, s := range b.Sources {
		zs, ok := s.(*zai.Source)
		if !ok {
			t.Fatalf("%T", s)
		}
		zs.Transport = rt
	}
	clock := &laneClock{time.Unix(1767229200, 0)} // 2026-01-01T01:00:00Z
	st, err := core.OpenState(t.TempDir(), clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sched := core.NewScheduler(b.Sources, st, clock, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute)

	// --once without --once-with-quota never reaches the vendor
	sched.Tick(context.Background(), OnceFilter(false), false, true)
	if rt.count() != 0 {
		t.Fatalf("%d requests without quota", rt.count())
	}
	sched.Tick(context.Background(), nil, false, true)
	clock.at = clock.at.Add(6 * time.Second) // the group spacing between the two accounts
	sched.Tick(context.Background(), nil, false, true)
	var out bytes.Buffer
	e := core.Exposition{Host: cfg.Host, Sched: sched, Clock: clock, Profiles: b.Profiles}
	if err := e.Render(&out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		`agent_quota_utilization_ratio{host="node-a",harness="zai",profile="zai-claw",account="zai-main",provider="zai",window="7d",scope="all"} 0.625`,
		`agent_quota_utilization_ratio{host="node-a",harness="zai",profile="zai-helper",account="zai-second",provider="zai",window="5h",scope="all"} 0.37`,
		`agent_quota_used_calls{host="node-a",harness="zai",profile="zai-claw",account="zai-main",provider="zai",window="monthly",scope="tool_calls"} 42`,
		`agent_account_info{host="node-a",harness="zai",profile="zai-claw",account="zai-main",provider="zai",plan="pro"} 1`,
		`agent_source_up{host="node-a",harness="zai",profile="zai-claw",source="zai-usage"} 1`,
		`agent_quota_exporter_profiles{harness="zai",origin="config"} 2`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s\n%s", want, got)
		}
	}
	if rt.count() != 2 {
		t.Fatalf("%d requests, want one per account", rt.count())
	}
}
