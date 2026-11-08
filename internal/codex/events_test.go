package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/events"
)

func TestEventTotalQuotaFailureRecovery(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	line := `{"timestamp":"2026-11-07T00:00:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"output_tokens":4,"reasoning_output_tokens":2},"total_token_usage":{"input_tokens":100,"output_tokens":40,"reasoning_output_tokens":20}},"rate_limits":{"primary":{"used_percent":31,"window_minutes":300,"resets_at":1900000000},"credits":{"has_credits":true,"unlimited":false,"balance":"12.5"}}}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "sessions", "rollout-a.jsonl"), []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	fail := true
	store, err := events.Open(events.Options{Root: t.TempDir(), Budget: 1 << 20, Reserve: 1, Write: func(f *os.File, b []byte) (int, error) {
		if fail {
			return 0, errors.New("injected")
		}
		return f.Write(b)
	}})
	if err != nil {
		t.Fatal(err)
	}
	s := &RolloutsSource{Host: "node-a", Alias: "profile-a", Dir: dir, Events: store}
	res, state, err := s.Collect(context.Background(), time.Date(2026, 11, 7, 0, 0, 1, 0, time.UTC), nil)
	if err != nil || len(res.Samples) == 0 {
		t.Fatalf("event failure stopped metrics: samples=%d err=%v", len(res.Samples), err)
	}
	if s.Events.WriteOutcomes()[events.Writer{Host: "node-a", Harness: Harness, Profile: "profile-a", Source: SourceName}] {
		t.Fatal("write failure hidden by health")
	}
	for _, c := range s.st.Events {
		if c.Offset != 0 {
			t.Fatal("cursor advanced on failure")
		}
	}
	fail = false
	s = &RolloutsSource{Host: "node-a", Alias: "profile-a", Dir: dir, Events: store}
	_, state, err = s.Collect(context.Background(), time.Now(), state)
	if err != nil {
		t.Fatal(err)
	}
	// Lost cursor checkpoint must replay without conflicting collected_at or duplicate rows.
	s = &RolloutsSource{Host: "node-a", Alias: "profile-a", Dir: dir, Events: store}
	if _, _, err = s.Collect(context.Background(), time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	if err := store.Replay(func(raw json.RawMessage) error {
		var e struct {
			Kind string         `json:"kind"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			return err
		}
		kinds[e.Kind]++
		if e.Kind == "usage" {
			total := e.Data["raw_total_tokens"].(map[string]any)
			if total["output"] != float64(40) || total["reasoning"] != float64(20) {
				t.Fatal(total)
			}
		}
		if e.Kind == "quota_observation" {
			w := e.Data["windows"].([]any)[0].(map[string]any)
			if w["original_percent"] != float64(31) {
				t.Fatal(w)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if kinds["usage"] != 1 || kinds["quota_observation"] != 1 {
		t.Fatal(kinds)
	}
	_ = state
}

func TestEventReaderBoundedOverlongAndPartial(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", maxLine+100)+"\nnext\npartial"), readBuf)
	line, n, err := boundedEventLine(r)
	if err != nil || line != "" || n != maxLine+101 {
		t.Fatalf("overlong retained: len=%d n=%d err=%v", len(line), n, err)
	}
	line, _, err = boundedEventLine(r)
	if err != nil || line != "next\n" {
		t.Fatal("failed to drain overlong record")
	}
	_, _, err = boundedEventLine(r)
	if err == nil {
		t.Fatal("partial line accepted")
	}
}

func TestEventTagsSubsetsAndCopiedReplay(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sessions", "rollout-a.jsonl")
	content := `{"type":"session_meta","payload":{"id":"session-a","cwd":"/home/example/repo","cli_version":"1"}}
{"type":"turn_context","payload":{"model":"Original-Model","cwd":"/home/example/worktree"}}
{"timestamp":"2026-11-07T00:00:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":20,"cache_write_input_tokens":5,"output_tokens":40,"reasoning_output_tokens":15,"total_tokens":140}}}}
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := events.Open(events.Options{Root: t.TempDir(), Budget: 1 << 20, Reserve: 1})
	if err != nil {
		t.Fatal(err)
	}
	s := &RolloutsSource{Host: "node-a", Alias: "profile-a", Dir: dir, Events: store}
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	_, state, err := s.Collect(context.Background(), now, nil)
	if err != nil {
		t.Fatal(err)
	}
	// An archive copy imported into the existing rollout scope must not add another event.
	if err := os.WriteFile(filepath.Join(dir, "sessions", "rollout-copy.jsonl"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	s = &RolloutsSource{Host: "node-a", Alias: "profile-a", Dir: dir, Events: store}
	if _, _, err := s.Collect(context.Background(), now.Add(time.Minute), state); err != nil {
		t.Fatal(err)
	}
	count := 0
	if err := store.Replay(func(raw json.RawMessage) error {
		count++
		var e struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			return err
		}
		d := e.Data
		if d["project"] != nil || d["request_id"] != nil || d["account"] != nil {
			t.Fatalf("guessed tags: %v", d)
		}
		if d["session_id"] != "session-a" || d["cwd"] != "/home/example/worktree" || d["model"] != "Original-Model" {
			t.Fatalf("lost tags: %v", d)
		}
		tokens := d["tokens"].(map[string]any)
		if tokens["input"] != float64(75) || tokens["output"] != float64(25) || tokens["reasoning"] != float64(15) {
			t.Fatalf("subset accounting: %v", tokens)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("events=%d, want 1", count)
	}
}

func TestEventMissingTagsAndBucketsNull(t *testing.T) {
	s := &RolloutsSource{Host: "node-a", Alias: "profile-a"}
	raw, _, err := s.rolloutEvent([]byte(`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"output_tokens":10}}}}`), &eventFile{ID: "file-a", Tags: map[string]any{}}, "rollout-a.jsonl", 100, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var e struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"session_id", "cwd", "project", "model"} {
		if e.Data[k] != nil {
			t.Fatalf("%s not null", k)
		}
	}
	tokens := e.Data["tokens"].(map[string]any)
	if tokens["output"] != nil || tokens["input"] != nil || tokens["reasoning"] != nil {
		t.Fatalf("invented buckets: %v", tokens)
	}
}
