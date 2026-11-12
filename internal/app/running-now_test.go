package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/schedtest"
)

const runningCanary = "CANARY-RUNNING-NOW"

// TestRunningNowEndToEnd drives the three "what is running now" sources through the real wiring: Claude sessions
// (a cli session, an sdk observer, a spare, one waiting session per reason and one with a canary reason), a real
// flock on a lock file read through the real /proc/locks, a call log full of canaries, and an opencode server.
// It compares the byte-exact output with a golden file and scans every label value.
func TestRunningNowEndToEnd(t *testing.T) {
	if _, err := os.Stat("/proc/locks"); err != nil {
		t.Skip("no /proc/locks")
	}
	defer stubLiveness()()
	root := t.TempDir()

	// Claude sessions
	home := filepath.Join(root, "home", "acct-a")
	dir := filepath.Join(home, ".claude")
	put(t, filepath.Join(home, ".claude.json"), fx(t, "dot-claude.json"), false)
	sess := func(pid int, fields ...string) {
		put(t, filepath.Join(dir, "sessions", fmt.Sprintf("%d.json", pid)),
			[]byte(fmt.Sprintf(`{"pid":%d,"sessionId":%q,"cwd":%q,"messagingSocketPath":%q,%s}`, pid, runningCanary, "/"+runningCanary, "/tmp/"+runningCanary, strings.Join(fields, ","))), false)
	}
	const common = `"kind":"interactive","entrypoint":"cli","startedAt":1767225000000`
	sess(101, `"name":"proj-aa"`, common, `"status":"busy"`, `"statusUpdatedAt":1767225540000`, `"tmux":"main:@0.%0"`)
	sess(102, `"name":"observer-1"`, `"kind":"interactive"`, `"entrypoint":"sdk-cli"`, `"startedAt":1767225100000`, `"status":"busy"`, `"statusUpdatedAt":1767225500000`)
	sess(103, `"name":"warm"`, common, `"status":"idle"`, `"spare":true`)
	for i, w := range []string{"permission prompt", "input needed", "dialog open", "sandbox request", "worker request", "goal proposal", runningCanary} {
		sess(110+i, fmt.Sprintf(`"name":"wait-%d"`, i), `"kind":"interactive","entrypoint":"cli"`, fmt.Sprintf(`"startedAt":%d`, 1767225200000+i*1000),
			`"status":"waiting"`, `"waitingFor":"`+w+`"`, `"statusUpdatedAt":1767225560000`)
	}
	sess(120, `"name":"napping"`, common, `"status":"idle"`, `"statusUpdatedAt":1767225300000`)
	sess(121, `"name":"shelled"`, common, `"status":"shell"`, `"statusUpdatedAt":1767225300000`)

	// jobs: one lock really held, one free, one with no profile anywhere
	locks := filepath.Join(root, "locks")
	if err := os.MkdirAll(locks, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"agy-acct-busy.lock", "agy-acct-free.lock", "codex-team.lock"} {
		put(t, filepath.Join(locks, n), nil, false)
	}
	lf, err := os.Open(filepath.Join(locks, "agy-acct-busy.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "calls.jsonl")
	row := func(tool, acct, ts string, dur float64, exit int) string {
		return fmt.Sprintf(`{"ts":%q,"caller":%q,"tool":%q,"account":%q,"cwd":%q,"session_id":%q,"task_sha256":%q,"task_length":3,"duration_s":%v,"exit_code":%d}`+"\n",
			ts, runningCanary, tool, acct, "/"+runningCanary, runningCanary, runningCanary, dur, exit)
	}
	put(t, log, []byte(row("agy", "acct-free", "2026-01-01T00:00:00Z", 12.5, 0)+row("codex", "team", "2026-01-01T00:01:00Z", 30, 2)+
		row("agy", "acct-free", "2026-01-01T00:02:00Z", 1, 75)), false)

	// opencode
	oc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"` + runningCanary + `":{"type":"busy"},"ses_2":{"type":"retry","message":"` + runningCanary + `"},"ses_3":{"type":"idle"}}`))
	}))
	defer oc.Close()
	ocDir := filepath.Join(root, "oc")
	os.MkdirAll(ocDir, 0o755)

	cfg := config.Default()
	cfg.Host = "node-a"
	cfg.StateDir = filepath.Join(root, "state")
	cfg.Discovery.Mode = "off"
	cfg.Metrics.PerSession = true
	cfg.Jobs = config.Jobs{LockDir: locks, CallLog: log}
	cfg.Profiles = []config.Profile{
		{Harness: "claude-code", Alias: "work", Dir: dir},
		{Harness: "opencode", Alias: "inst", Dir: ocDir, URL: oc.URL},
	}
	profiles, _ := cfg.Resolve(func(string) string { return "" })
	b, err := Build(cfg, profiles, cfg.Host, "test")
	if err != nil {
		t.Fatal(err)
	}
	var srcs []core.Source
	for _, s := range b.Sources {
		switch s.Name() {
		case "claude-sessions", "job-locks", "opencode-http":
			srcs = append(srcs, s)
		}
	}
	if len(srcs) != 3 {
		t.Fatalf("wired %d of the three sources", len(srcs))
	}
	h := schedtest.New(t, t0.Add(10*time.Minute), srcs...)
	h.Tick(time.Second)
	out := h.Render(t)

	golden := filepath.Join("..", "..", "testdata", "golden", "running-now.txt")
	if *update {
		os.WriteFile(golden, []byte(out), 0o644)
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Fatalf("/metrics differs from golden (run go test ./internal/app -run RunningNow -update after checking the diff):\n%s", out)
	}
	if strings.Contains(out, "CANARY") {
		t.Fatal("a canary reached /metrics")
	}
	checkContract(t, out)

	// label values: no path, no pid-like number, no session id, no free text
	val := regexp.MustCompile(`([a-z_]+)="([^"]*)"`)
	for _, ln := range strings.Split(out, "\n") {
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		for _, m := range val.FindAllStringSubmatch(ln, -1) {
			v := m[2]
			if strings.ContainsAny(v, "/\\") || regexp.MustCompile(`^\d{3,}$`).MatchString(v) || len(v) > 64 {
				t.Fatalf("label %s=%q looks like a path, pid or free text in %s", m[1], v, ln)
			}
		}
	}
}

func TestOneShotRunSkipsARemoteOpencodeServerButKeepsLoopback(t *testing.T) {
	cfg := config.Default()
	cfg.Discovery.Mode = "off"
	cfg.Profiles = []config.Profile{
		{Harness: "opencode", Alias: "near", Dir: t.TempDir(), URL: "http://127.0.0.1:4096"},
		{Harness: "opencode", Alias: "far", Dir: t.TempDir(), URL: "http://10.0.0.5:4096", AllowRemote: true},
	}
	profiles, _ := cfg.Resolve(func(string) string { return "" })
	b, err := Build(cfg, profiles, "node-a", "test")
	if err != nil {
		t.Fatal(err)
	}
	keep := OnceFilter(false)
	got := map[string]bool{}
	for _, s := range b.Sources {
		if s.Name() == "opencode-http" {
			got[s.Profile()] = keep(s)
		}
	}
	if !got["near"] || got["far"] || len(got) != 2 {
		t.Fatalf("one-shot filter: %v", got)
	}
}
