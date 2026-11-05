package agy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
)

// The fake agy client is this test binary itself, reached through a symlink named "agy": a shell script would
// add its own variables (PWD, SHLVL, _) to the environment it reports and could not trap SIGTERM while a
// foreground sleep runs. Its behaviour is chosen by files in the HOME it is given, since it gets no other
// environment.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "agy" {
		os.Exit(fakeAgy())
	}
	os.Exit(m.Run())
}

func fakeAgy() int {
	home := os.Getenv("HOME")
	if len(os.Args) > 1 && os.Args[1] == "grandchild" { // left behind by mode "linger", holding stdout open
		time.Sleep(30 * time.Second)
		return 0
	}
	_ = os.WriteFile(filepath.Join(home, "uid.txt"), []byte(strconv.Itoa(os.Getuid())), 0o600)
	mode := "ok"
	if b, err := os.ReadFile(filepath.Join(home, "mode")); err == nil {
		mode = strings.TrimSpace(string(b))
	}
	var sig chan os.Signal
	if mode == "term" || mode == "ignore" {
		sig = make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM)
	}
	env := os.Environ()
	sort.Strings(env)
	cwd, _ := os.Getwd()
	_ = os.WriteFile(filepath.Join(home, "env.txt"), []byte(strings.Join(env, "\n")), 0o600)
	_ = os.WriteFile(filepath.Join(home, "cwd.txt"), []byte(cwd), 0o600)
	_ = os.WriteFile(filepath.Join(home, "args.txt"), []byte(strings.Join(os.Args[1:], " ")), 0o600)
	f, _ := os.OpenFile(filepath.Join(home, "ran"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("ran\n")
	f.Close()

	// serialisation probe: a lock file shared by several fake clients; finding it taken means two runs overlap
	if b, err := os.ReadFile(filepath.Join(home, "shared")); err == nil {
		lock := filepath.Join(string(b), "lock")
		lf, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = os.WriteFile(filepath.Join(string(b), "overlap"), []byte("x"), 0o600)
		} else {
			lf.Close()
			time.Sleep(300 * time.Millisecond)
			os.Remove(lock)
		}
	}

	switch mode {
	case "fail":
		return 3
	case "term": // finishes its work on SIGTERM and exits cleanly
		select {
		case <-sig:
			_ = os.WriteFile(filepath.Join(home, "term"), []byte("x"), 0o600)
			return 0
		case <-time.After(30 * time.Second):
			return 0
		}
	case "ignore": // notes SIGTERM and keeps running: only SIGKILL ends it
		go func() {
			<-sig
			_ = os.WriteFile(filepath.Join(home, "term"), []byte("x"), 0o600)
		}()
		time.Sleep(30 * time.Second)
		return 0
	case "big":
		os.Stdout.Write(make([]byte, maxStdout+1))
		return 0
	case "linger": // exits at once, leaving a grandchild in its process group that keeps stdout open
		gc := exec.Command(os.Args[0], "grandchild")
		gc.Env, gc.Stdout = os.Environ(), os.Stdout
		if gc.Start() != nil {
			return 4
		}
		_ = os.WriteFile(filepath.Join(home, "grandchild.pid"), []byte(strconv.Itoa(gc.Process.Pid)), 0o600)
	}
	b, _ := os.ReadFile(filepath.Join(home, "out.json"))
	os.Stdout.Write(b)
	return 0
}

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "agy", "usage-command.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// mutate returns the fixture with one change applied to its decoded form.
func mutate(t *testing.T, f func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(fixture(t), &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	b, _ := json.Marshal(m)
	return b
}

func fakeBin(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "agy")
	if err := os.Symlink(exe, bin); err != nil {
		t.Fatal(err)
	}
	return bin
}

// newSource builds the source through Build, as production does, with the fake client injected.
func newSource(t *testing.T, mode string, out []byte) (*UsageSource, string) {
	t.Helper()
	home := t.TempDir()
	write(t, filepath.Join(home, "mode"), []byte(mode))
	write(t, filepath.Join(home, "out.json"), out)
	srcs := Build(config.Default(), config.Resolved{Harness: config.HarnessAgy, Alias: "p1", Dir: home, Account: "acct-a1",
		Quota: config.Quota{"agy_usage": true}}, "node-a")
	if len(srcs) != 1 {
		t.Fatalf("Build returned %d sources", len(srcs))
	}
	s := srcs[0].(*UsageSource)
	s.Bin = fakeBin(t)
	return s, home
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func sourceErr(t *testing.T, err error) *core.SourceError {
	t.Helper()
	var se *core.SourceError
	if !errors.As(err, &se) {
		t.Fatalf("want a SourceError, got %v", err)
	}
	return se
}

func render(ss []core.Sample) []string {
	var out []string
	for _, s := range ss {
		var ls []string
		for _, l := range s.Labels {
			ls = append(ls, l.Name+"="+l.Value)
		}
		out = append(out, fmt.Sprintf("%s{%s} %.6g", s.Name, strings.Join(ls, ","), s.Value))
	}
	sort.Strings(out)
	return out
}

func ts(s string) string {
	tm, _ := time.Parse(time.RFC3339, s)
	return fmt.Sprintf("%.6g", float64(tm.Unix()))
}

func noCanary(t *testing.T, things ...string) {
	t.Helper()
	for _, s := range things {
		if strings.Contains(s, "CANARY") {
			t.Errorf("content field leaked: %q", s)
		}
	}
}

func TestBuild(t *testing.T) {
	cfg := config.Default()
	r := config.Resolved{Harness: config.HarnessAgy, Alias: "p1", Dir: "/x", Account: "acct-a1"}
	if got := Build(cfg, r, "node-a"); got != nil {
		t.Fatalf("no opt-in must build nothing, got %d sources", len(got))
	}
	r.Quota = config.Quota{"agy_usage": false}
	if got := Build(cfg, r, "node-a"); got != nil {
		t.Fatal("agy_usage: false must build nothing")
	}
	r.Quota["agy_usage"] = true
	s := Build(cfg, r, "node-a")[0].(*UsageSource)
	if s.Name() != "agy-usage" || s.Harness() != "agy" || s.Profile() != "p1" || !s.Network() || s.NetGroup() != "agy" {
		t.Errorf("identity: %s %s %s network=%v group=%s", s.Name(), s.Harness(), s.Profile(), s.Network(), s.NetGroup())
	}
	if s.MinInterval() != 30*time.Minute || s.backoff() != 60*time.Minute || s.NetSpacing() != 10*time.Second {
		t.Errorf("defaults: interval %s backoff %s", s.MinInterval(), s.backoff())
	}
	if s.ReadTimeout() <= s.timeout()+s.grace() {
		t.Errorf("read timeout %s must cover a run and its grace", s.ReadTimeout())
	}
	cfg.Sources = map[string]config.Source{"agy-usage": {MinInterval: config.Duration(20 * time.Minute), Backoff429: config.Duration(2 * time.Hour)}}
	s = Build(cfg, r, "node-a")[0].(*UsageSource)
	if s.MinInterval() != 20*time.Minute || s.backoff() != 2*time.Hour {
		t.Errorf("config override ignored: interval %s backoff %s", s.MinInterval(), s.backoff())
	}
}

func TestGoldenFromFixture(t *testing.T) {
	s, home := newSource(t, "ok", fixture(t))
	res, blob, err := s.Collect(context.Background(), t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := "host=node-a,harness=agy,profile=p1,account=acct-a1,provider=google"
	q := func(name, window, scope, v string) string {
		return fmt.Sprintf("%s{%s,window=%s,scope=%s} %s", name, base, window, scope, v)
	}
	want := []string{
		q("agent_quota_utilization_ratio", "5h", "3p", "0"),
		q("agent_quota_utilization_ratio", "weekly", "3p", "0"),
		q("agent_quota_utilization_ratio", "5h", "gemini", "0.1"),
		q("agent_quota_utilization_ratio", "weekly", "gemini", "0.5"),
		q("agent_quota_utilization_ratio", "other", "gemini", "0.25"), // window "Per Minute!" is not label-safe
		q("agent_quota_utilization_ratio", "weekly", "other", "0.8"),  // two unknown ids merged: max utilization
		q("agent_quota_reset_timestamp_seconds", "5h", "3p", ts("2030-01-01T05:00:00Z")),
		q("agent_quota_reset_timestamp_seconds", "weekly", "3p", ts("2030-01-08T00:00:00Z")),
		q("agent_quota_reset_timestamp_seconds", "5h", "gemini", ts("2030-01-01T05:00:00Z")),
		q("agent_quota_reset_timestamp_seconds", "weekly", "gemini", ts("2030-01-08T00:00:00Z")),
		q("agent_quota_reset_timestamp_seconds", "other", "gemini", ts("2030-01-01T00:01:00Z")),
		q("agent_quota_reset_timestamp_seconds", "weekly", "other", ts("2030-01-08T00:00:00Z")), // min reset
		fmt.Sprintf("agent_quota_observed_timestamp_seconds{%s} %.6g", base, float64(t0.Unix())),
	}
	sort.Strings(want)
	got := render(res.Samples)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("samples:\n got %s\nwant %s", strings.Join(got, "\n     "), strings.Join(want, "\n     "))
	}
	if !res.ObservedAt.Equal(t0) {
		t.Errorf("ObservedAt %s, want the call time %s", res.ObservedAt, t0)
	}
	if res.Errors[core.ReasonParse] != 2 || len(res.Errors) != 1 {
		t.Errorf("unknown buckets must count once each under parse, got %v", res.Errors)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(home, "args.txt"))); got != "-p /usage --output-format json" {
		t.Errorf("argv %q", got)
	}
	noCanary(t, append(got, string(blob))...)
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestChildEnvIsMinimal(t *testing.T) {
	t.Setenv("AGY_TEST_INHERITED", "must-not-pass")
	s, home := newSource(t, "ok", fixture(t))
	if _, _, err := s.Collect(context.Background(), t0, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"HOME=" + home, "LANG=C", "PATH=" + os.Getenv("PATH")}
	if got := strings.Split(readFile(t, filepath.Join(home, "env.txt")), "\n"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("child env\n got %q\nwant %q", got, want)
	}
	cwd, _ := filepath.EvalSymlinks(readFile(t, filepath.Join(home, "cwd.txt")))
	if h, _ := filepath.EvalSymlinks(home); cwd != h {
		t.Errorf("child cwd %q, want the profile HOME %q", cwd, h)
	}
}

func TestMissingAccountNeverSpawns(t *testing.T) {
	s, home := newSource(t, "ok", fixture(t))
	s.Account = ""
	res, blob, err := s.Collect(context.Background(), t0, nil)
	if se := sourceErr(t, err); se.Reason != core.ReasonOther {
		t.Errorf("reason %s, want other", se.Reason)
	}
	if len(res.Samples) != 0 || blob != nil {
		t.Error("no account must yield no samples and no state")
	}
	if exists(filepath.Join(home, "ran")) {
		t.Error("the client was spawned without an account")
	}
	if r := s.Restore(json.RawMessage(`{"last":[{"w":"5h","s":"3p","r":0.1}],"last_at":1,"acct":""}`)); len(r.Samples) != 0 {
		t.Error("Restore without an account must serve nothing")
	}
}

func TestGuardAgainstSpentTurns(t *testing.T) {
	cases := map[string]func(m map[string]any){
		"num_turns 1":       func(m map[string]any) { m["num_turns"] = 1 },
		"num_turns missing": func(m map[string]any) { delete(m, "num_turns") },
		"total_tokens 7":    func(m map[string]any) { m["usage"].(map[string]any)["total_tokens"] = 7 },
		"usage missing":     func(m map[string]any) { delete(m, "usage") },
		"usage a string":    func(m map[string]any) { m["usage"] = "0" },
		"command chat":      func(m map[string]any) { m["command"].(map[string]any)["name"] = "chat" },
		"command missing":   func(m map[string]any) { delete(m, "command") },
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			s, _ := newSource(t, "ok", mutate(t, f))
			res, blob, err := s.Collect(context.Background(), t0, nil)
			se := sourceErr(t, err)
			if se.Reason != core.ReasonOther || se.Backoff != GuardBackoff || GuardBackoff < 24*time.Hour {
				t.Errorf("got reason %s backoff %s, want other and a backoff of at least a day", se.Reason, se.Backoff)
			}
			if len(res.Samples) != 0 || blob != nil {
				t.Error("guarded output must give no samples and no state")
			}
			noCanary(t, err.Error())
		})
	}
}

func TestNonZeroExitBacksOffAndServesLastGood(t *testing.T) {
	s, home := newSource(t, "ok", fixture(t))
	good, blob, err := s.Collect(context.Background(), t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, "mode"), []byte("fail"))
	res, nblob, err := s.Collect(context.Background(), t0.Add(time.Hour), blob)
	se := sourceErr(t, err)
	if se.Reason != core.ReasonUpstream || se.Backoff != 60*time.Minute {
		t.Errorf("reason %s backoff %s, want upstream and 60m", se.Reason, se.Backoff)
	}
	if nblob != nil {
		t.Error("a failed run must keep the persisted reading")
	}
	if !res.ObservedAt.Equal(t0) || strings.Join(render(res.Samples), "\n") != strings.Join(render(good.Samples), "\n") {
		t.Error("the last good reading, with its own time, must be served alongside the error")
	}
}

func TestUnparseableOutput(t *testing.T) {
	for name, out := range map[string][]byte{
		"not json":   []byte("Usage: agy [options]\n"),
		"empty":      nil,
		"no buckets": mutate(t, func(m map[string]any) { m["command"].(map[string]any)["data"] = map[string]any{"groups": []any{}} }),
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newSource(t, "ok", out)
			if _, _, err := s.Collect(context.Background(), t0, nil); sourceErr(t, err).Reason != core.ReasonParse {
				t.Errorf("want parse, got %v", err)
			}
		})
	}
	s, _ := newSource(t, "big", nil)
	if _, _, err := s.Collect(context.Background(), t0, nil); sourceErr(t, err).Reason != core.ReasonParse {
		t.Errorf("output over the cap: want parse, got %v", err)
	}
}

func TestTimeoutSendsSIGTERMFirst(t *testing.T) {
	s, home := newSource(t, "term", fixture(t))
	s.Timeout, s.Grace = time.Second, 10*time.Second
	start := time.Now()
	_, _, err := s.Collect(context.Background(), t0, nil)
	el := time.Since(start)
	se := sourceErr(t, err)
	if se.Reason != core.ReasonTimeout || se.Backoff != 60*time.Minute {
		t.Errorf("reason %s backoff %s, want timeout with the backoff", se.Reason, se.Backoff)
	}
	if !exists(filepath.Join(home, "term")) {
		t.Error("the client never saw SIGTERM")
	}
	if el >= s.Grace {
		t.Errorf("a client that exits on SIGTERM must not be held for the grace (took %s)", el)
	}
}

func TestTimeoutKillsAfterGrace(t *testing.T) {
	s, home := newSource(t, "ignore", fixture(t))
	s.Timeout, s.Grace = time.Second, 500*time.Millisecond
	start := time.Now()
	_, _, err := s.Collect(context.Background(), t0, nil)
	el := time.Since(start)
	if sourceErr(t, err).Reason != core.ReasonTimeout {
		t.Errorf("want timeout, got %v", err)
	}
	if !exists(filepath.Join(home, "term")) {
		t.Error("SIGTERM must come before SIGKILL")
	}
	if el < s.Timeout+s.Grace || el > 10*time.Second {
		t.Errorf("returned after %s: want SIGKILL only after timeout + grace, and promptly then", el)
	}
}

func TestRunsAreSerialised(t *testing.T) {
	shared := t.TempDir()
	var srcs []*UsageSource
	for i := 0; i < 3; i++ {
		s, home := newSource(t, "ok", fixture(t))
		write(t, filepath.Join(home, "shared"), []byte(shared))
		srcs = append(srcs, s)
	}
	var wg sync.WaitGroup
	errs := make([]error, len(srcs))
	for i, s := range srcs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = s.Collect(context.Background(), t0, nil)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("source %d: %v", i, err)
		}
	}
	if exists(filepath.Join(shared, "overlap")) {
		t.Error("two agy clients ran at the same time")
	}
}

func TestRestoreServesPersistedReading(t *testing.T) {
	s, _ := newSource(t, "ok", fixture(t))
	res, blob, err := s.Collect(context.Background(), t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	fresh := &UsageSource{Host: s.Host, Alias: s.Alias, Dir: s.Dir, Account: s.Account, Interval: s.Interval}
	r := fresh.Restore(blob)
	if !r.ObservedAt.Equal(t0) {
		t.Errorf("restored ObservedAt %s, want the original call time", r.ObservedAt)
	}
	if strings.Join(render(r.Samples), "\n") != strings.Join(render(res.Samples), "\n") {
		t.Errorf("restored samples differ:\n%s", strings.Join(render(r.Samples), "\n"))
	}
	fresh.Account = "acct-b2"
	if r := fresh.Restore(blob); len(r.Samples) != 0 {
		t.Error("a reading of another account must not be restored under this profile")
	}
}

func TestMissingClientIsNotFound(t *testing.T) {
	s, home := newSource(t, "ok", fixture(t))
	s.Bin = ""
	t.Setenv("PATH", t.TempDir())
	if _, _, err := s.Collect(context.Background(), t0, nil); sourceErr(t, err).Reason != core.ReasonNotFound {
		t.Errorf("want not_found, got %v", err)
	}
	if exists(filepath.Join(home, "ran")) {
		t.Error("nothing may run when agy is not on PATH")
	}
}

// gone reports whether pid has exited: no such process, or a zombie its new parent has not reaped yet.
func gone(pid int) bool {
	if syscall.Kill(pid, 0) == syscall.ESRCH {
		return true
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
	return len(f) > 0 && f[0] == "Z"
}

func TestLingeringGrandchildIsReaped(t *testing.T) {
	s, home := newSource(t, "linger", fixture(t))
	s.Grace = 300 * time.Millisecond
	_, _, _ = s.Collect(context.Background(), t0, nil) // the result does not matter here, only what is left running
	pid, err := strconv.Atoi(strings.TrimSpace(readFile(t, filepath.Join(home, "grandchild.pid"))))
	if err != nil || pid <= 0 {
		t.Fatalf("grandchild pid: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(2 * time.Second)
	for !gone(pid) {
		if time.Now().After(deadline) {
			t.Fatal("a grandchild holding stdout outlived Collect: the process group was not cleared")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ownedTree builds, under a world-traversable root of its own, a copy of the fake client and a HOME owned by uid,
// so the client can run as that user (t.TempDir and the go build cache may be closed to other users).
func ownedTree(t *testing.T, uid int) (bin, home string) {
	t.Helper()
	root, err := os.MkdirTemp("", "agy-owner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	bin = filepath.Join(root, "agy")
	out, err := os.OpenFile(bin, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	home = filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, "out.json"), fixture(t))
	for _, p := range []string{home, filepath.Join(home, "out.json")} {
		if err := os.Chown(p, uid, uid); err != nil {
			t.Fatal(err)
		}
	}
	return bin, home
}

func ownedSource(t *testing.T, uid int) (*UsageSource, string) {
	t.Helper()
	bin, home := ownedTree(t, uid)
	s := Build(config.Default(), config.Resolved{Harness: config.HarnessAgy, Alias: "p1", Dir: home, Account: "acct-a1",
		Quota: config.Quota{"agy_usage": true}}, "node-a")[0].(*UsageSource)
	s.Bin = bin
	return s, home
}

func TestClientRunsAsHomeOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("switching the client's user needs root")
	}
	const nobody = 65534
	for _, owner := range []int{nobody, 0} {
		t.Run(strconv.Itoa(owner), func(t *testing.T) {
			s, home := ownedSource(t, owner)
			if _, _, err := s.Collect(context.Background(), t0, nil); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, filepath.Join(home, "uid.txt")); got != strconv.Itoa(owner) {
				t.Errorf("client ran as uid %s, want the HOME's owner %d", got, owner)
			}
			// what the client wrote in the HOME belongs to the owner, as a token-file rewrite would
			fi, err := os.Stat(filepath.Join(home, "uid.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if st := fi.Sys().(*syscall.Stat_t); st.Uid != uint32(owner) || st.Gid != uint32(owner) {
				t.Errorf("file written by the client is owned by %d:%d, want %d:%d", st.Uid, st.Gid, owner, owner)
			}
		})
	}
}

func TestHomeMustBeARealDirectory(t *testing.T) {
	s, home := newSource(t, "ok", fixture(t))
	link := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	s.Dir = link
	if _, _, err := s.Collect(context.Background(), t0, nil); sourceErr(t, err).Reason != core.ReasonPermission {
		t.Errorf("HOME behind a link: want permission, got %v", err)
	}
	s.Dir = filepath.Join(home, "out.json")
	if _, _, err := s.Collect(context.Background(), t0, nil); sourceErr(t, err).Reason != core.ReasonPermission {
		t.Errorf("HOME is a file: want permission, got %v", err)
	}
	s.Dir = filepath.Join(home, "missing")
	if _, _, err := s.Collect(context.Background(), t0, nil); sourceErr(t, err).Reason != core.ReasonNotFound {
		t.Errorf("HOME missing: want not_found, got %v", err)
	}
	if exists(filepath.Join(home, "ran")) {
		t.Error("the client was spawned without a valid HOME")
	}
}

func TestRunAs(t *testing.T) {
	for _, c := range []struct {
		euid, owner, want uint32
		ok                bool
	}{
		{0, 0, 0, true},             // root exporter, root's own HOME: root
		{0, 1000, 1000, true},       // root exporter switches to the owner
		{1000, 1000, 1000, true},    // the exporter's own HOME
		{1000, 1001, 0, false},      // another user's HOME: a non-root exporter cannot switch
		{1000, 0, 0, false},         // root's HOME from a non-root exporter
		{65534, 65534, 65534, true}, // any non-root exporter in its own HOME
	} {
		got, ok := runAs(c.euid, c.owner)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("runAs(%d, %d) = %d, %v; want %d, %v", c.euid, c.owner, got, ok, c.want, c.ok)
		}
	}
}
