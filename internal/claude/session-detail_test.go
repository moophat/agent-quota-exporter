package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/schedtest"
)

const waitCanary = "CANARY-WAITING-TEXT"

// sessionHarness is a profile with no session files, liveness stubbed to "alive" and a scheduler over a
// per-session source.
func sessionHarness(t *testing.T, perSession bool, max int) (*Profile, *schedtest.H) {
	t.Helper()
	t.Cleanup(stubLiveness())
	p := newProfile(t, "a")
	if err := os.RemoveAll(filepath.Join(p.Dir, "sessions")); err != nil {
		t.Fatal(err)
	}
	src := &SessionsSource{P: p, Interval: 15 * time.Second, PerSession: perSession, MaxSessions: max}
	return p, schedtest.New(t, t0, src)
}

// putSession writes a session file; fields are JSON fragments, e.g. `"status":"busy"`.
func putSession(t *testing.T, p *Profile, pid int, fields ...string) {
	t.Helper()
	body := fmt.Sprintf(`{"pid":%d,%s}`, pid, strings.Join(fields, ","))
	write(t, filepath.Join(p.Dir, "sessions", fmt.Sprintf("%d.json", pid)), []byte(body))
}

func has(out, line string) bool { return strings.Contains(out, line+"\n") }

func TestPerSessionStateAndSinceInSeconds(t *testing.T) {
	p, h := sessionHarness(t, true, 0)
	putSession(t, p, 11, `"name":"proj-1a"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"busy"`,
		`"startedAt":1794505816213`, `"statusUpdatedAt":1794506043142`, `"tmux":"orchestrator:@0.%0"`)
	h.Tick(time.Second)
	out := h.Render(t)
	base := `host="node-a",harness="claude-code",profile="a",account="`
	for _, want := range []string{
		`agent_session_state{` + base,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in\n%s", want, out)
		}
	}
	for _, st := range []string{"busy", "idle", "waiting", "shell", "other"} {
		want := 0
		if st == "busy" {
			want = 1
		}
		line := fmt.Sprintf(`session="proj-1a",state="%s"} %d`, st, want)
		if !strings.Contains(out, line) {
			t.Fatalf("want %s in\n%s", line, out)
		}
	}
	// milliseconds in the file, seconds in the series
	if !strings.Contains(out, `session="proj-1a"} 1.794506043142e+09`) {
		t.Fatalf("since not in seconds:\n%s", out)
	}
	if !strings.Contains(out, `session="proj-1a",entrypoint="cli",tmux_session="orchestrator"} 1`) {
		t.Fatalf("info row missing:\n%s", out)
	}
}

func TestWaitingReasonsAreAnExactClosedSet(t *testing.T) {
	p, h := sessionHarness(t, true, 0)
	cases := map[string]string{"permission prompt": "permission", "input needed": "input", "dialog open": "dialog",
		"sandbox request": "sandbox", "worker request": "worker", "goal proposal": "goal",
		waitCanary: "other", "permission prompt and more": "other", "Permission Prompt": "other", "": "other"}
	pid := 100
	names := map[string]string{}
	for text, reason := range cases {
		pid++
		name := fmt.Sprintf("s%d", pid)
		names[name] = reason
		putSession(t, p, pid, `"name":"`+name+`"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"waiting"`,
			`"startedAt":1000`, `"waitingFor":"`+text+`"`)
	}
	h.Tick(time.Second)
	out := h.Render(t)
	for name, reason := range names {
		for _, r := range waitingReasons {
			want := 0
			if r == reason {
				want = 1
			}
			line := fmt.Sprintf(`session="%s",reason="%s"} %d`, name, r, want)
			if !strings.Contains(out, line) {
				t.Fatalf("want %s", line)
			}
		}
		if !strings.Contains(out, fmt.Sprintf(`session="%s",state="waiting"} 1`, name)) {
			t.Fatalf("%s not waiting", name)
		}
	}
	if strings.Contains(out, waitCanary) || strings.Contains(out, "and more") {
		t.Fatal("free waitingFor text reached /metrics")
	}
}

func TestNotWaitingHasNoReason(t *testing.T) {
	p, h := sessionHarness(t, true, 0)
	putSession(t, p, 5, `"name":"x"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"busy"`, `"waitingFor":"input needed"`)
	h.Tick(time.Second)
	out := h.Render(t)
	if strings.Contains(out, `reason="input"} 1`) {
		t.Fatalf("stale waitingFor on a busy session exported a reason:\n%s", out)
	}
}

func TestReusedPidIsNotLiveAnywhere(t *testing.T) {
	p, h := sessionHarness(t, true, 0)
	putSession(t, p, 7, `"name":"gone"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"busy"`, `"procStart":"111"`)
	ProcStart = func(int) (string, bool) { return "222", true } // the pid now belongs to another process
	h.Tick(time.Second)
	out := h.Render(t)
	if strings.Contains(out, `session="gone"`) {
		t.Fatalf("session of a reused pid exported:\n%s", out)
	}
	for _, st := range []string{"active", "idle", "other"} {
		if !strings.Contains(out, `account="`) || !has2(out, `state="`+st+`"} 0`) {
			t.Fatalf("agent_sessions %s must be 0 for a reused pid:\n%s", st, out)
		}
	}
	// the same file with the right start time is live
	ProcStart = func(int) (string, bool) { return "111", true }
	h.Tick(time.Second)
	if !strings.Contains(h.Render(t), `session="gone",state="busy"} 1`) {
		t.Fatal("matching procStart must be live")
	}
}

func has2(out, frag string) bool { return strings.Contains(out, frag) }

func TestPidDomainOfAnotherHostIsSkipped(t *testing.T) {
	p, h := sessionHarness(t, true, 0)
	PidDomain = func() string { return "linux:machine-a:pid:[4026531836]" }
	putSession(t, p, 8, `"name":"other-host"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"busy"`,
		`"pidDomain":"linux:machine-b:pid:[4026531836]"`)
	putSession(t, p, 9, `"name":"this-host"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"busy"`,
		`"pidDomain":"linux:machine-a:pid:[4026531836]"`)
	h.Tick(time.Second)
	out := h.Render(t)
	if strings.Contains(out, `session="other-host"`) || !strings.Contains(out, `session="this-host",state="busy"} 1`) {
		t.Fatalf("pid domain not compared as a whole:\n%s", out)
	}
	if !strings.Contains(out, `state="active"} 1`) {
		t.Fatal("agent_sessions must count only this host's session")
	}
}

func TestProcStatWithSpacesAndParenthesesInCommand(t *testing.T) {
	// fields after the last ")" start at field 3; field 22 is the 20th of them
	rest := strings.Repeat("0 ", 18) + "987654 0 0"
	got, ok := startFromStat([]byte("123 (we ird) name) S " + rest))
	if !ok || got != "987654" {
		t.Fatalf("got %q %v", got, ok)
	}
	if _, ok := startFromStat([]byte("123 (short) S 1 2")); ok {
		t.Fatal("short stat accepted")
	}
}

func TestSparesAndOtherKindsAreSkipped(t *testing.T) {
	p, h := sessionHarness(t, true, 0)
	putSession(t, p, 21, `"name":"spare-one"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"idle"`, `"spare":true`)
	putSession(t, p, 22, `"name":"job-run"`, `"kind":"job"`, `"entrypoint":"cli"`, `"status":"busy"`)
	putSession(t, p, 23, `"name":"no-kind"`, `"entrypoint":"cli"`, `"status":"busy"`)
	putSession(t, p, 24, `"name":"bg-one"`, `"kind":"bg"`, `"entrypoint":"cli"`, `"status":"idle"`)
	h.Tick(time.Second)
	out := h.Render(t)
	for _, n := range []string{"spare-one", "job-run", "no-kind"} {
		if strings.Contains(out, `session="`+n+`"`) {
			t.Fatalf("%s exported", n)
		}
	}
	if !strings.Contains(out, `session="bg-one",state="idle"} 1`) {
		t.Fatal("bg session missing")
	}
}

func TestEntrypointLabels(t *testing.T) {
	p, h := sessionHarness(t, true, 0)
	putSession(t, p, 31, `"name":"a"`, `"kind":"interactive"`, `"entrypoint":"sdk-cli"`, `"status":"busy"`)
	putSession(t, p, 32, `"name":"b"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"busy"`)
	putSession(t, p, 33, `"name":"c"`, `"kind":"interactive"`, `"entrypoint":"weird"`, `"status":"busy"`)
	h.Tick(time.Second)
	out := h.Render(t)
	for _, w := range []string{`session="a",entrypoint="sdk",`, `session="b",entrypoint="cli",`, `session="c",entrypoint="other",`} {
		if !strings.Contains(out, w) {
			t.Fatalf("missing %s", w)
		}
	}
	if strings.Contains(out, "sdk-cli") || strings.Contains(out, "weird") {
		t.Fatal("raw entrypoint leaked")
	}
}

func TestOverflowKeepsCliThenOldestAndCountsDrops(t *testing.T) {
	p, h := sessionHarness(t, true, 3)
	// two sdk (old), four cli at different ages
	putSession(t, p, 41, `"name":"sdk-old"`, `"kind":"interactive"`, `"entrypoint":"sdk-cli"`, `"status":"busy"`, `"startedAt":1`)
	putSession(t, p, 42, `"name":"cli-3"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"busy"`, `"startedAt":30`)
	putSession(t, p, 43, `"name":"cli-1"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"busy"`, `"startedAt":10`)
	putSession(t, p, 44, `"name":"cli-2"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"busy"`, `"startedAt":20`)
	putSession(t, p, 45, `"name":"cli-4"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"busy"`, `"startedAt":40`)
	putSession(t, p, 46, `"name":"sdk-new"`, `"kind":"interactive"`, `"entrypoint":"sdk-cli"`, `"status":"busy"`, `"startedAt":50`)
	h.Tick(time.Second)
	out := h.Render(t)
	for _, n := range []string{"cli-1", "cli-2", "cli-3"} {
		if !strings.Contains(out, `session="`+n+`",state="busy"} 1`) {
			t.Fatalf("%s should be kept", n)
		}
	}
	for _, n := range []string{"cli-4", "sdk-old", "sdk-new"} {
		if strings.Contains(out, `session="`+n+`"`) {
			t.Fatalf("%s should be dropped", n)
		}
	}
	if !strings.Contains(out, `reason="other"} 3`) {
		t.Fatalf("overflow not counted as source errors:\n%s", out)
	}
	// counts still cover every live session
	if !strings.Contains(out, `state="active"} 6`) {
		t.Fatalf("agent_sessions must count all live sessions:\n%s", out)
	}
}

func TestDuplicateAndUnsafeNames(t *testing.T) {
	p, h := sessionHarness(t, true, 0)
	putSession(t, p, 51, `"name":"same"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"busy"`, `"startedAt":2`)
	putSession(t, p, 52, `"name":"same"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"busy"`, `"startedAt":1`)
	putSession(t, p, 53, `"name":"same"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"busy"`, `"startedAt":3`)
	putSession(t, p, 54, `"name":"a b/c\"d"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"idle"`)
	putSession(t, p, 55, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"idle"`)
	putSession(t, p, 56, `"name":"`+strings.Repeat("x", 80)+`"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"idle"`)
	h.Tick(time.Second)
	out := h.Render(t)
	for _, n := range []string{"same", "same-2", "same-3", "a-b-c-d", "unnamed", strings.Repeat("x", 48)} {
		if !strings.Contains(out, `session="`+n+`",state="busy"}`) && !strings.Contains(out, `session="`+n+`",state="idle"}`) {
			t.Fatalf("missing session %q in\n%s", n, out)
		}
	}
	// older start keeps the plain name: pid 52 (startedAt 1) is "same"
	if !strings.Contains(out, `session="same",state="busy"} 1`) {
		t.Fatal("plain name not on the oldest")
	}
}

func TestTmuxSessionIsBounded(t *testing.T) {
	for in, want := range map[string]string{"orchestrator:@0.%0": "orchestrator", "": "", "bad name:@1.%2": "",
		"x/y:@0.%0": "", strings.Repeat("a", 49) + ":@0.%0": "", "noColon": "noColon"} {
		if got := tmuxSession(in); got != want {
			t.Errorf("tmuxSession(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPerSessionOffExportsNoSessionSeries(t *testing.T) {
	p, h := sessionHarness(t, false, 0)
	putSession(t, p, 61, `"name":"x"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"busy"`)
	h.Tick(time.Second)
	out := h.Render(t)
	if strings.Contains(out, "agent_session_") {
		t.Fatalf("per-session series without the opt-in:\n%s", out)
	}
	if !strings.Contains(out, `state="active"} 1`) {
		t.Fatal("counts missing")
	}
}

// A failed read must remove the per-session series and mark the source down, not serve the last good values for
// the stale window (a session list that cannot be read is unknown).
func TestFailedReadDropsSeriesAndMarksSourceDown(t *testing.T) {
	p, h := sessionHarness(t, true, 0)
	putSession(t, p, 71, `"name":"alive-one"`, `"kind":"interactive"`, `"entrypoint":"cli"`, `"status":"busy"`)
	h.Tick(time.Second)
	out := h.Render(t)
	if !strings.Contains(out, `session="alive-one",state="busy"} 1`) ||
		!strings.Contains(out, `agent_source_up{host="node-a",harness="claude-code",profile="a",source="claude-sessions"} 1`) {
		t.Fatalf("first read:\n%s", out)
	}
	// the sessions dir becomes a link: the read is refused
	dir := filepath.Join(p.Dir, "sessions")
	os.RemoveAll(dir)
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	h.Tick(time.Second)
	out = h.Render(t)
	if strings.Contains(out, "agent_session_state") || strings.Contains(out, "agent_sessions{") {
		t.Fatalf("series survived a failed read:\n%s", out)
	}
	if !strings.Contains(out, `agent_source_up{host="node-a",harness="claude-code",profile="a",source="claude-sessions"} 0`) {
		t.Fatalf("source must be down after one failed read:\n%s", out)
	}
}

func TestSessionsSourceErrorCarriesDropCache(t *testing.T) {
	p, _ := sessionHarness(t, true, 0)
	dir := filepath.Join(p.Dir, "sessions")
	os.RemoveAll(dir)
	os.Symlink(t.TempDir(), dir)
	_, _, err := collect(t, &SessionsSource{P: p, Interval: time.Minute}, t0, nil)
	se, ok := err.(*core.SourceError)
	if !ok || !se.DropCache {
		t.Fatalf("want DropCache source error, got %#v", err)
	}
}
