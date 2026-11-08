package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOnceWithQuotaRequiresOnce(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"--once-with-quota"}, &out, &errb); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "requires --once") {
		t.Fatalf("stderr = %q, want a message naming --once", errb.String())
	}
}

func TestEnabledEventStoreFailsStartup(t *testing.T) {
	for _, tc := range []struct{ name, root, budget, key, want string }{
		{"unwritable", "/proc/aqe-event-store-test", "1024", "", "events:"},
		{"budget", "/tmp/unused-events", "-1", "", "events.byte_budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			profile := ""
			if tc.name == "key" {
				profile = "profiles:\n  - harness: claude-code\n    alias: work\n    dir: " + dir + "\n"
			}
			body := "version: 1\ndiscovery: {mode: off}\nstate_dir: " + filepath.Join(dir, "state") + "\n" + profile +
				"events:\n  enabled: true\n  root_dir: " + tc.root + "\n  byte_budget: " + tc.budget + "\n" + tc.key
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			if code := run([]string{"--config", path, "--once"}, &out, &stderr); code == 0 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			if out.Len() != 0 {
				t.Fatal("startup failure emitted metrics")
			}
		})
	}
}
