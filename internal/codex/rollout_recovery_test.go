package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/events"
)

const recoveryUsage = `{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"output_tokens":4}},"rate_limits":{"primary":{"used_percent":31}}}}` + "\n"

func recoverySource(t *testing.T, content string) (*RolloutsSource, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sessions", "rollout-a.jsonl")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := events.Open(events.Options{Root: t.TempDir(), Budget: 64 << 20, Reserve: 1})
	if err != nil {
		t.Fatal(err)
	}
	s := &RolloutsSource{Host: "node-a", Alias: "profile-a", Dir: dir, Events: store}
	s.load(nil)
	return s, path
}
func recoveryCapture(t *testing.T, s *RolloutsSource) {
	t.Helper()
	files, err := s.list()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.captureEvents(context.Background(), files, time.Now()); err != nil {
		t.Fatal(err)
	}
}
func recoveryCount(t *testing.T, s *RolloutsSource, kind string) int {
	t.Helper()
	n := 0
	if err := s.Events.Replay(func(raw json.RawMessage) error {
		var e events.Envelope
		if err := json.Unmarshal(raw, &e); err != nil {
			return err
		}
		if e.Kind == kind {
			n++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return n
}
func TestOrdinaryCwd(t *testing.T) {
	s, _ := recoverySource(t, `{"type":"session_meta","payload":{"cwd":"/work/@team/task-app"}}`+"\n"+recoveryUsage)
	recoveryCapture(t, s)
	if n := recoveryCount(t, s, "usage"); n != 1 {
		t.Fatalf("usage=%d", n)
	}
}

func TestSecretCwdRedacted(t *testing.T) {
	s, _ := recoverySource(t, `{"type":"session_meta","payload":{"cwd":"/work/password=private"}}`+"\n"+recoveryUsage)
	recoveryCapture(t, s)
	if err := s.Events.Replay(func(raw json.RawMessage) error {
		if strings.Contains(string(raw), "password=") {
			t.Fatal("secret path retained")
		}
		var e struct {
			Kind    string         `json:"kind"`
			Data    map[string]any `json:"data"`
			Quality struct {
				Reasons []string `json:"reasons"`
			} `json:"quality"`
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			return err
		}
		if e.Kind == "usage" && (e.Data["cwd"] != nil || !strings.Contains(strings.Join(e.Quality.Reasons, ","), "secret_bearing_cwd")) {
			t.Fatal("missing redaction quality")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestLongLineProgress(t *testing.T) {
	s, _ := recoverySource(t, strings.Repeat("x", 17<<20)+"\n"+recoveryUsage)
	for i := 0; i < 6; i++ {
		recoveryCapture(t, s)
	}
	if n := recoveryCount(t, s, "usage"); n != 1 {
		t.Fatalf("later usage=%d, want 1", n)
	}
}
func TestRepeatedQuota(t *testing.T) {
	s, _ := recoverySource(t, recoveryUsage+recoveryUsage)
	recoveryCapture(t, s)
	if n := recoveryCount(t, s, "quota_observation"); n != 2 {
		t.Fatalf("observations=%d, want 2", n)
	}
}
func TestFileFairness(t *testing.T) {
	s, _ := recoverySource(t, strings.Repeat("x\n", 9<<20))
	if err := os.WriteFile(filepath.Join(s.Dir, "sessions", "rollout-z.jsonl"), []byte(recoveryUsage), 0600); err != nil {
		t.Fatal(err)
	}
	recoveryCapture(t, s)
	if n := recoveryCount(t, s, "usage"); n != 1 {
		t.Fatalf("later file usage=%d", n)
	}
}
func TestReplacementAfterDiscovery(t *testing.T) {
	s, path := recoverySource(t, "{}\n"+recoveryUsage)
	files, err := s.list()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"+recoveryUsage+recoveryUsage), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.captureEvents(context.Background(), files, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := recoveryCount(t, s, "usage"); n != 0 {
		t.Fatalf("stale discovery emitted %d replacement events", n)
	}
	recoveryCapture(t, s)
	if n := recoveryCount(t, s, "usage"); n != 2 {
		t.Fatalf("replacement usage=%d, want 2", n)
	}
}
func TestSameInodeRewrite(t *testing.T) {
	s, path := recoverySource(t, "{}\n"+recoveryUsage)
	recoveryCapture(t, s)
	if err := os.WriteFile(path, []byte("{ }\n"+recoveryUsage+recoveryUsage), 0600); err != nil {
		t.Fatal(err)
	}
	recoveryCapture(t, s)
	if n := recoveryCount(t, s, "usage"); n != 3 {
		t.Fatalf("rewritten usage=%d, want 3", n)
	}
}

func TestRewritePreservesMetadataAndSpan(t *testing.T) {
	meta := `{"type":"session_meta","payload":{"id":"session-a"}}` + "\n"
	for _, growth := range []bool{false, true} {
		t.Run(fmt.Sprint(growth), func(t *testing.T) {
			// Padding puts the changed record outside both bounded samples.
			padding := strings.Repeat(" \n", 3000)
			original := meta + padding + recoveryUsage + padding
			s, path := recoverySource(t, original)
			recoveryCapture(t, s)
			oldID := s.st.Events[hashKey("rollout-a.jsonl")].ID
			rewritten := strings.Replace(original, `"input_tokens":10`, `"input_tokens":20`, 1)
			if growth {
				// Change a sampled byte to prove a growing rewrite, while retaining
				// the first metadata line and the original usage byte span.
				rewritten = strings.Replace(rewritten, " \n", "\t\n", 1) + "{}\n"
			}
			time.Sleep(time.Millisecond)
			if err := os.WriteFile(path, []byte(rewritten), 0600); err != nil {
				t.Fatal(err)
			}
			recoveryCapture(t, s)
			c := s.st.Events[hashKey("rollout-a.jsonl")]
			if c.ID == oldID {
				t.Fatal("rewrite reused generation with unchanged metadata")
			}
			if n := recoveryCount(t, s, "quota_observation"); n != 2 {
				t.Fatalf("rewritten same-span quota observations=%d, want 2", n)
			}
			if n := recoveryCount(t, s, "usage"); n != 2 {
				t.Fatalf("rewritten usage=%d, want 2", n)
			}
			// Restart and ordinary append preserve the replacement generation.
			state, _ := json.Marshal(s.st)
			s = &RolloutsSource{Host: s.Host, Alias: s.Alias, Dir: s.Dir, Events: s.Events}
			s.load(state)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.WriteString(recoveryUsage); err != nil {
				t.Fatal(err)
			}
			f.Close()
			recoveryCapture(t, s)
			if s.st.Events[hashKey("rollout-a.jsonl")].ID != c.ID {
				t.Fatal("append changed generation")
			}
		})
	}
}
