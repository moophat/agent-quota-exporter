package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/schedtest"
)

var t0 = time.Unix(1767225600, 0).UTC()

const canary = "CANARY-JOB-SECRET"

type rig struct {
	lockDir, log, locks string
	src                 *Source
}

func newRig(t *testing.T) *rig {
	t.Helper()
	d := t.TempDir()
	r := &rig{lockDir: filepath.Join(d, "locks"), log: filepath.Join(d, "calls.jsonl"), locks: filepath.Join(d, "proc-locks")}
	if err := os.MkdirAll(r.lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(r.locks, nil, 0o600)
	r.src = &Source{Host: "node-a", LockDir: r.lockDir, CallLog: r.log, Interval: 5 * time.Second, LocksPath: r.locks}
	return r
}

func (r *rig) lock(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.lockDir, name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// hold writes a fake /proc/locks that lists the named lock files as held, plus a waiter line for the first.
func (r *rig) hold(t *testing.T, names ...string) {
	t.Helper()
	var b strings.Builder
	for i, n := range names {
		var st syscall.Stat_t
		if err := syscall.Stat(filepath.Join(r.lockDir, n), &st); err != nil {
			t.Fatal(err)
		}
		k := lockKey(st.Dev, st.Ino)
		var maj, min, ino uint64
		fmt.Sscanf(strings.ReplaceAll(k, ":", " "), "%d %d %d", &maj, &min, &ino)
		fmt.Fprintf(&b, "%d: FLOCK  ADVISORY  WRITE 9%d %02x:%02x:%d 0 EOF\n", i+1, i, maj, min, ino)
		if i == 0 {
			fmt.Fprintf(&b, "%d: -> FLOCK  ADVISORY  WRITE 99%d %02x:%02x:%d 0 EOF\n", i+1, i, maj, min, ino)
		}
	}
	os.WriteFile(r.locks, []byte(b.String()), 0o600)
}

func (r *rig) logRow(t *testing.T, tool, account string, ts time.Time, dur float64, exit int) {
	t.Helper()
	line := fmt.Sprintf(`{"ts":%q,"caller":%q,"tool":%q,"account":%q,"cwd":%q,"resume_id":null,"session_id":%q,"task_sha256":%q,"task_length":9,"duration_s":%v,"exit_code":%d}`+"\n",
		ts.UTC().Format(time.RFC3339Nano), canary, tool, account, canary, canary, canary, dur, exit)
	f, err := os.OpenFile(r.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(line)
	f.Close()
}

func (r *rig) render(t *testing.T) (*schedtest.H, string) {
	t.Helper()
	h := schedtest.New(t, t0, r.src)
	h.Tick(time.Second)
	return h, h.Render(t)
}

const lab = `{host="node-a",harness="%s",profile="%s",account="%s"} `

func line(name, tool, acct string) string { return name + fmt.Sprintf(lab, tool, acct, acct) }

func TestRunningFromLockTableForAccountsWithoutAnyProfile(t *testing.T) {
	r := newRig(t)
	r.lock(t, "agy-acct-one.lock")
	r.lock(t, "agy-acct-two.lock")
	r.lock(t, "codex-team.lock")
	r.hold(t, "agy-acct-two.lock") // one holder, one waiter line for the same file
	_, out := r.render(t)
	for want, v := range map[string]string{
		line("agent_job_running", "agy", "acct-one"): "0",
		line("agent_job_running", "agy", "acct-two"): "1",
		line("agent_job_running", "codex", "team"):   "0",
	} {
		if !strings.Contains(out, want+v+"\n") {
			t.Fatalf("want %s%s in\n%s", want, v, out)
		}
	}
}

func TestWaiterLineDoesNotCountAsHolding(t *testing.T) {
	r := newRig(t)
	r.lock(t, "agy-a.lock")
	var st syscall.Stat_t
	syscall.Stat(filepath.Join(r.lockDir, "agy-a.lock"), &st)
	var maj, min, ino uint64
	fmt.Sscanf(strings.ReplaceAll(lockKey(st.Dev, st.Ino), ":", " "), "%d %d %d", &maj, &min, &ino)
	os.WriteFile(r.locks, []byte(fmt.Sprintf("1: -> FLOCK  ADVISORY  WRITE 5 %02x:%02x:%d 0 EOF\n2: POSIX  ADVISORY  WRITE 6 %02x:%02x:%d 0 EOF\n", maj, min, ino, maj, min, ino)), 0o600)
	_, out := r.render(t)
	if !strings.Contains(out, line("agent_job_running", "agy", "a")+"0\n") {
		t.Fatalf("waiter or POSIX line counted as holding:\n%s", out)
	}
}

func TestLockTableParsing(t *testing.T) {
	b := []byte("1: FLOCK  ADVISORY  WRITE 968 00:19:1359 0 EOF\n2: -> FLOCK  ADVISORY  WRITE 970 00:19:1360 0 EOF\n" +
		"3: FLOCK  ADVISORY  READ 12 fd:0a:5 0 EOF\n4: OFDLCK ADVISORY  READ -1 08:01:9 0 EOF\ngarbage\n")
	got := parseLocks(b)
	if len(got) != 2 || !got["0:25:1359"] || !got["253:10:5"] {
		t.Fatalf("parsed %v", got)
	}
	// device number encoding: major 253 minor 10, and a minor above 255
	if k := lockKey(64778, 5); k != "253:10:5" {
		t.Fatalf("lockKey = %s", k)
	}
	if k := lockKey(uint64(8<<8)|44|(256<<12), 7); k != "8:300:7" {
		t.Fatalf("lockKey high minor = %s", k)
	}
}

// The real kernel lists a flock held by this process, and releasing it clears the row. Nothing in the source takes
// the flock, so a second descriptor can still lock the free file afterwards.
func TestRealKernelFlock(t *testing.T) {
	if _, err := os.Stat(procLocks); err != nil {
		t.Skip("no /proc/locks")
	}
	r := newRig(t)
	r.src.LocksPath = "" // the real table
	r.lock(t, "agy-live.lock")
	f, err := os.Open(filepath.Join(r.lockDir, "agy-live.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	h := schedtest.New(t, t0, r.src)
	h.Tick(time.Second)
	if out := h.Render(t); !strings.Contains(out, line("agent_job_running", "agy", "live")+"1\n") {
		t.Fatalf("held lock not seen:\n%s", out)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	h.Tick(time.Second)
	if out := h.Render(t); !strings.Contains(out, line("agent_job_running", "agy", "live")+"0\n") {
		t.Fatalf("released lock still running:\n%s", out)
	}
	g, err := os.Open(filepath.Join(r.lockDir, "agy-live.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := syscall.Flock(int(g.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("the source left the lock taken: %v", err)
	}
}

func TestOnlyRunnerLockNamesAreListed(t *testing.T) {
	r := newRig(t)
	r.lock(t, "agy-ok.lock")
	for _, bad := range []string{"evil.lock", "agy-.lock", "agy-bad name.lock", "codex-.hidden.lock", "agy-ok.lock.bak", "other-x.lock"} {
		r.lock(t, bad)
	}
	os.Mkdir(filepath.Join(r.lockDir, "agy-dir.lock"), 0o700)
	os.Symlink("/etc/hostname", filepath.Join(r.lockDir, "agy-link.lock"))
	_, out := r.render(t)
	if n := strings.Count(out, "agent_job_running{"); n != 1 {
		t.Fatalf("want exactly one lock listed, got %d:\n%s", n, out)
	}
}

func TestLockFileCapIsCounted(t *testing.T) {
	r := newRig(t)
	for i := 0; i < maxLocks+5; i++ {
		r.lock(t, fmt.Sprintf("agy-a%03d.lock", i))
	}
	_, out := r.render(t)
	if n := strings.Count(out, "agent_job_running{"); n != maxLocks {
		t.Fatalf("listed %d", n)
	}
	if !strings.Contains(out, `source="job-locks",reason="other"} 5`) {
		t.Fatalf("overflow not counted:\n%s", out)
	}
}

func TestLastEndAndExitFromCallLog(t *testing.T) {
	r := newRig(t)
	r.lock(t, "agy-a.lock")
	r.lock(t, "codex-team.lock")
	start := t0.Add(-time.Hour)
	r.logRow(t, "agy", "a", start, 30.5, 0)
	r.logRow(t, "agy", "a", start.Add(time.Minute), 10, 1)     // later call, later end
	r.logRow(t, "agy", "a", start.Add(2*time.Minute), 0.5, 75) // busy reject: not a job
	r.logRow(t, "agy", "a", start.Add(3*time.Minute), 0, 124)  // wait timeout: not a job
	r.logRow(t, "agy", "ghost", start, 5, 0)                   // no lock file
	r.logRow(t, "other", "a", start, 5, 0)                     // unknown tool
	r.logRow(t, "agy", "../x", start, 5, 0)                    // invalid account string
	_, out := r.render(t)
	wantEnd := "1767222070" // call start (t0 - 1h) + 1 minute + 10 s duration
	if !strings.Contains(out, line("agent_job_last_end_timestamp_seconds", "agy", "a")+wantEnd+"\n") {
		t.Fatalf("end time: want %s in\n%s", wantEnd, out)
	}
	if !strings.Contains(out, line("agent_job_last_exit_code", "agy", "a")+"1\n") {
		t.Fatalf("exit code:\n%s", out)
	}
	if strings.Contains(out, "ghost") || strings.Contains(out, `harness="other"`) {
		t.Fatal("rows without a lock or tool exported")
	}
	if strings.Contains(out, "last_end_timestamp_seconds{host=\"node-a\",harness=\"codex\"") {
		t.Fatal("end time for an account with no logged job")
	}
}

func TestCallLogContentNeverLeaks(t *testing.T) {
	r := newRig(t)
	r.lock(t, "agy-a.lock")
	r.logRow(t, "agy", "a", t0.Add(-time.Minute), 3, 0)
	h, out := r.render(t)
	_ = h
	if strings.Contains(out, canary) {
		t.Fatal("call-log content in /metrics")
	}
	_, blob, err := r.src.Collect(context.Background(), t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), canary) {
		t.Fatalf("call-log content in persisted state: %s", blob)
	}
}

func TestLogIsReadIncrementallyAndSurvivesTruncation(t *testing.T) {
	r := newRig(t)
	r.lock(t, "agy-a.lock")
	start := t0.Add(-time.Hour)
	r.logRow(t, "agy", "a", start, 1, 0)
	_, blob, err := r.src.Collect(context.Background(), t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var st state
	json.Unmarshal(blob, &st)
	fi, _ := os.Stat(r.log)
	if st.Off != fi.Size() {
		t.Fatalf("offset %d, size %d", st.Off, fi.Size())
	}
	// a partial line stays unread until it is complete
	f, _ := os.OpenFile(r.log, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"ts":"` + start.Add(time.Minute).UTC().Format(time.RFC3339Nano) + `","tool":"agy","account":"a","duration_s":2,`)
	f.Close()
	res, blob2, _ := r.src.Collect(context.Background(), t0, blob)
	json.Unmarshal(blob2, &st)
	if st.Off != fi.Size() {
		t.Fatalf("partial line consumed: %d", st.Off)
	}
	f, _ = os.OpenFile(r.log, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`"exit_code":3}` + "\n")
	f.Close()
	res, blob3, _ := r.src.Collect(context.Background(), t0, blob2)
	if v := find(res.Samples, "agent_job_last_exit_code"); v != 3 {
		t.Fatalf("completed line not read, exit %v", v)
	}
	// the log is replaced by a shorter one holding a newer job: it is read from its start
	os.WriteFile(r.log, []byte(`{"ts":"`+start.Add(time.Hour).UTC().Format(time.RFC3339Nano)+`","tool":"agy","account":"a","duration_s":1,"exit_code":9}`+"\n"), 0o600)
	res, blob4, _ := r.src.Collect(context.Background(), t0, blob3)
	if v := find(res.Samples, "agent_job_last_exit_code"); v != 9 {
		t.Fatalf("replaced log not read from its start, exit %v", v)
	}
	json.Unmarshal(blob4, &st)
	fi, _ = os.Stat(r.log)
	if st.Off != fi.Size() {
		t.Fatalf("offset after replacement %d, size %d", st.Off, fi.Size())
	}
}

func TestFirstRunReadsOnlyTheTail(t *testing.T) {
	r := newRig(t)
	r.lock(t, "agy-a.lock")
	pad := strings.Repeat("x", 90)
	f, _ := os.Create(r.log)
	// an old row (outside the tail window) followed by padding past 8 MiB, then a recent row
	f.WriteString(fmt.Sprintf(`{"ts":"2026-01-01T00:00:00Z","tool":"agy","account":"a","duration_s":1,"exit_code":7}` + "\n"))
	for written := 0; written < firstReadMax+1<<20; written += len(pad) + 40 {
		fmt.Fprintf(f, `{"ts":"2026-01-02T00:00:00Z","tool":"x","pad":"%s"}`+"\n", pad)
	}
	f.WriteString(`{"ts":"2026-02-01T00:00:00Z","tool":"agy","account":"a","duration_s":1,"exit_code":4}` + "\n")
	f.Close()
	res, _, err := r.src.Collect(context.Background(), t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := find(res.Samples, "agent_job_last_exit_code"); v != 4 {
		t.Fatalf("recent row not read, exit %v", v)
	}
	if res.Errors["parse"] != 0 {
		t.Fatalf("the cut line was counted as malformed: %v", res.Errors)
	}
}

func TestMalformedAndOversizedLinesAreCounted(t *testing.T) {
	r := newRig(t)
	r.lock(t, "agy-a.lock")
	f, _ := os.Create(r.log)
	f.WriteString("{not json\n")
	f.WriteString(strings.Repeat("y", maxLine+10) + "\n")
	f.Close()
	r.logRow(t, "agy", "a", t0.Add(-time.Minute), 1, 0)
	res, _, err := r.src.Collect(context.Background(), t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors["parse"] != 2 {
		t.Fatalf("parse errors %v", res.Errors)
	}
	if v := find(res.Samples, "agent_job_last_exit_code"); v != 0 {
		t.Fatalf("good row after bad ones lost: %v", v)
	}
}

// A lock dir or lock table that cannot be read leaves the state unknown: through the scheduler the series vanish
// and the source is down after one failed read, instead of serving the last good values.
func TestFailedReadsDropSeriesThroughScheduler(t *testing.T) {
	for name, breakIt := range map[string]func(r *rig){
		"lock dir gone":    func(r *rig) { os.RemoveAll(r.lockDir) },
		"lock table gone":  func(r *rig) { os.Remove(r.locks) },
		"lock dir is link": func(r *rig) { os.RemoveAll(r.lockDir); os.Symlink(t.TempDir(), r.lockDir) },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.lock(t, "agy-a.lock")
			r.hold(t, "agy-a.lock")
			h, out := r.render(t)
			up := `agent_source_up{host="node-a",harness="jobs",profile="host",source="job-locks"} `
			if !strings.Contains(out, line("agent_job_running", "agy", "a")+"1\n") || !strings.Contains(out, up+"1\n") {
				t.Fatalf("first read:\n%s", out)
			}
			breakIt(r)
			h.Tick(time.Second)
			out = h.Render(t)
			if strings.Contains(out, "agent_job_") {
				t.Fatalf("job series survived a failed read:\n%s", out)
			}
			if !strings.Contains(out, up+"0\n") {
				t.Fatalf("source not down:\n%s", out)
			}
		})
	}
}

func TestAbsentLockDirIsNotFound(t *testing.T) {
	r := newRig(t)
	os.RemoveAll(r.lockDir)
	_, _, err := r.src.Collect(context.Background(), t0, nil)
	if err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("got %v", err)
	}
}

func find(ss []core.Sample, name string) float64 {
	for _, s := range ss {
		if s.Name == name {
			return s.Value
		}
	}
	return -1
}

// A rotated log is a different file: its rows are read from the start whether it is shorter or longer than the
// offset into the old one (a longer one used to be entered in the middle of a line).
func TestRotatedLogIsReadFromItsStart(t *testing.T) {
	for name, extra := range map[string]int{"shorter": 0, "longer": 40} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.lock(t, "agy-a.lock")
			r.logRow(t, "agy", "a", t0.Add(-3*time.Hour), 1, 1)
			_, blob, err := r.src.Collect(context.Background(), t0, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(r.log, r.log+".1"); err != nil {
				t.Fatal(err)
			}
			rows := 1
			if extra > 0 {
				rows = 3
			}
			for i := 0; i < rows; i++ {
				dur := 1.0
				if i == 0 && extra > 0 {
					dur = 1.123456789012345 // a longer first line, so the old offset falls inside it
				}
				r.logRow(t, "agy", "a", t0.Add(-time.Hour+time.Duration(i)*time.Minute), dur, 5+i)
			}
			res, _, err := r.src.Collect(context.Background(), t0, blob)
			if err != nil {
				t.Fatal(err)
			}
			if v := find(res.Samples, "agent_job_last_exit_code"); v != float64(5+rows-1) {
				t.Fatalf("exit %v, want the last row of the new log (%d)", v, 5+rows-1)
			}
			if res.Errors["parse"] != 0 {
				t.Fatalf("entered the new log mid-line: %v", res.Errors)
			}
		})
	}
}

// An account outside the exported first 100 keeps its last job, so it still has it when it moves inside.
func TestHistoryIsKeptForAccountsBeyondTheExportCap(t *testing.T) {
	r := newRig(t)
	for i := 0; i < maxLocks+5; i++ {
		r.lock(t, fmt.Sprintf("agy-a%03d.lock", i))
	}
	last := fmt.Sprintf("a%03d", maxLocks+4)
	r.logRow(t, "agy", last, t0.Add(-time.Hour), 7, 3)
	_, blob, err := r.src.Collect(context.Background(), t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ { // ten earlier accounts disappear: the last one moves inside the cap
		os.Remove(filepath.Join(r.lockDir, fmt.Sprintf("agy-a%03d.lock", i)))
	}
	res, _, err := r.src.Collect(context.Background(), t0, blob)
	if err != nil {
		t.Fatal(err)
	}
	if v := find(res.Samples, "agent_job_last_exit_code"); v != 3 {
		t.Fatalf("history lost while the account was beyond the cap, exit %v", v)
	}
}

func TestHugeLockDirectoryExportsTheCapAndCountsTheRest(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 2500; i++ {
		r.lock(t, fmt.Sprintf("agy-b%04d.lock", i))
		r.lock(t, fmt.Sprintf("noise-%04d.txt", i))
	}
	res, _, err := r.src.Collect(context.Background(), t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := countSamples(res.Samples, "agent_job_running"); n != maxLocks {
		t.Fatalf("exported %d", n)
	}
	if res.Errors["other"] != 2500-maxLocks {
		t.Fatalf("overflow %v", res.Errors)
	}
	if !hasAccount(res.Samples, "b0000") || !hasAccount(res.Samples, "b0099") || hasAccount(res.Samples, "b0100") {
		t.Fatal("the smallest names must be the exported ones")
	}
}

func countSamples(ss []core.Sample, name string) int {
	n := 0
	for _, s := range ss {
		if s.Name == name {
			n++
		}
	}
	return n
}

func hasAccount(ss []core.Sample, acct string) bool {
	for _, s := range ss {
		for _, l := range s.Labels {
			if l.Name == "account" && l.Value == acct {
				return true
			}
		}
	}
	return false
}

// A lock table over the read bound is refused: through the scheduler the running series vanish and the source is
// down, rather than held locks beyond the bound reading as 0.
func TestOversizedLockTableMarksSourceDown(t *testing.T) {
	defer func(v int64) { maxLockTable = v }(maxLockTable)
	r := newRig(t)
	r.lock(t, "agy-a.lock")
	r.hold(t, "agy-a.lock")
	h, out := r.render(t)
	if !strings.Contains(out, line("agent_job_running", "agy", "a")+"1\n") {
		t.Fatalf("first read:\n%s", out)
	}
	maxLockTable = 10
	h.Tick(time.Second)
	out = h.Render(t)
	if strings.Contains(out, "agent_job_") || !strings.Contains(out, `source="job-locks"} 0`) {
		t.Fatalf("truncated table accepted:\n%s", out)
	}
}

// On filesystems where a lock file's device differs from the one /proc/locks prints, a held lock could never be
// matched: the source must say it does not know instead of reporting every job as stopped.
func TestFilesystemWhereLocksCannotBeMatchedMarksSourceDown(t *testing.T) {
	defer func(f func(*os.File) (int64, error)) { fsType = f }(fsType)
	r := newRig(t)
	r.lock(t, "agy-a.lock")
	h, out := r.render(t)
	if !strings.Contains(out, line("agent_job_running", "agy", "a")) {
		t.Fatalf("first read:\n%s", out)
	}
	for name, magic := range map[string]int64{"btrfs": 0x9123683e, "overlayfs": 0x794c7630, "nfs": 0x6969} {
		fsType = func(*os.File) (int64, error) { return magic, nil }
		h.Tick(time.Second)
		out = h.Render(t)
		if strings.Contains(out, "agent_job_") || !strings.Contains(out, `source="job-locks"} 0`) {
			t.Fatalf("%s accepted:\n%s", name, out)
		}
	}
}

// An unterminated line over the length limit is counted once and skipped as it grows: the offset moves past it
// instead of the same bytes being scanned every cycle, and the next complete line is read.
func TestOverlongUnfinishedLogLineIsSkippedNotRescanned(t *testing.T) {
	r := newRig(t)
	r.lock(t, "agy-a.lock")
	os.WriteFile(r.log, []byte(strings.Repeat("z", maxLine+500)), 0o600)
	res, blob, err := r.src.Collect(context.Background(), t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var st state
	json.Unmarshal(blob, &st)
	if st.Off != maxLine+500 || res.Errors["parse"] != 1 {
		t.Fatalf("offset %d, errors %v", st.Off, res.Errors)
	}
	f, _ := os.OpenFile(r.log, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(strings.Repeat("z", 100)) // the writer is still on the same line
	f.Close()
	res, blob, _ = r.src.Collect(context.Background(), t0, blob)
	json.Unmarshal(blob, &st)
	if st.Off != maxLine+600 || res.Errors["parse"] != 0 {
		t.Fatalf("growing line rescanned or recounted: offset %d, errors %v", st.Off, res.Errors)
	}
	f, _ = os.OpenFile(r.log, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("\n")
	f.Close()
	r.logRow(t, "agy", "a", t0.Add(-time.Minute), 1, 4)
	res, _, _ = r.src.Collect(context.Background(), t0, blob)
	if v := find(res.Samples, "agent_job_last_exit_code"); v != 4 || res.Errors["parse"] != 0 {
		t.Fatalf("row after the long line: exit %v, errors %v", v, res.Errors)
	}
}
