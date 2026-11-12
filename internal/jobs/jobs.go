// Package jobs reads which agent jobs are running now from the files a task runner leaves on the host
// (`job-locks`, specs/sources.md, "Jobs").
//
// The convention it reads: while a job for account A of tool T runs, the runner holds an exclusive flock on the
// empty file <lock_dir>/T-A.lock; every call appends one JSON line to a call log. The source never takes the
// flock (it reads /proc/locks, which lists who holds which lock), so it cannot make a runner wait or fail. From the
// call log it keeps only the end time and exit code of the last finished call per account; task text, caller,
// directory, session id and the task hash are not decoded.
package jobs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/safefs"
)

const (
	// SourceName is the contract `source` value.
	SourceName = "job-locks"
	// Harness and ProfileAlias label the source's own health series; the job series carry the tool and account.
	Harness      = "jobs"
	ProfileAlias = "host"

	maxLocks     = 100     // lock files read per cycle; the rest count as source errors
	firstReadMax = 8 << 20 // bytes of call log read on the first run
	maxLine      = 64 << 10
	procLocks    = "/proc/locks"
)

// lockName is the runner's own rule for an account lock file: tool prefix and a bounded account name.
var lockName = regexp.MustCompile(`^(codex|agy)-([A-Za-z0-9][A-Za-z0-9_.-]{0,63})\.lock$`)
var accountName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// Source is `job-locks` for one host.
type Source struct {
	Host     string
	LockDir  string
	CallLog  string // optional; empty = no end time or exit code
	Interval time.Duration
	// LocksPath is /proc/locks; a test points it at a fixture.
	LocksPath string
}

func (s *Source) Name() string               { return SourceName }
func (s *Source) Harness() string            { return Harness }
func (s *Source) Profile() string            { return ProfileAlias }
func (s *Source) MinInterval() time.Duration { return s.Interval }
func (s *Source) Network() bool              { return false }

type last struct {
	End  float64 `json:"end"`  // Unix seconds
	Exit int     `json:"exit"` // exit code
}

type state struct {
	Off  int64           `json:"off"` // call-log bytes consumed
	Last map[string]last `json:"last"`
}

// fail marks a read the source cannot vouch for: the series vanish and the source reports down.
func fail(reason string) error { return &core.SourceError{Reason: reason, DropCache: true} }

type lockFile struct{ tool, account, key string }

func (s *Source) Collect(_ context.Context, _ time.Time, raw json.RawMessage) (core.Result, json.RawMessage, error) {
	res := core.Result{}
	st := state{Last: map[string]last{}}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &st)
		if st.Last == nil {
			st.Last = map[string]last{}
		}
	}
	locks, over, err := s.listLocks()
	if err != nil {
		return res, nil, err
	}
	if over > 0 {
		res.Errors = map[string]int{core.ReasonOther: over}
	}
	held, err := heldLocks(s.locksPath())
	if err != nil {
		return res, nil, err
	}
	have := map[string]bool{}
	for _, l := range locks {
		have[l.tool+"/"+l.account] = true
	}
	for k := range st.Last {
		if !have[k] {
			delete(st.Last, k) // the account's lock file is gone; its history goes with it
		}
	}
	if s.CallLog != "" {
		parse, err := s.readLog(&st, have)
		if err != nil {
			return res, nil, err
		}
		if parse > 0 {
			if res.Errors == nil {
				res.Errors = map[string]int{}
			}
			res.Errors[core.ReasonParse] += parse
		}
	}
	for _, l := range locks {
		lb := []core.Label{{Name: "host", Value: s.Host}, {Name: "harness", Value: l.tool},
			{Name: "profile", Value: l.account}, {Name: "account", Value: l.account}}
		run := 0.0
		if held[l.key] {
			run = 1
		}
		res.Samples = append(res.Samples, core.Sample{Name: "agent_job_running", Labels: lb, Value: run})
		if x, ok := st.Last[l.tool+"/"+l.account]; ok {
			res.Samples = append(res.Samples,
				core.Sample{Name: "agent_job_last_end_timestamp_seconds", Labels: lb, Value: x.End},
				core.Sample{Name: "agent_job_last_exit_code", Labels: lb, Value: float64(x.Exit)})
		}
	}
	out, _ := json.Marshal(st)
	return res, out, nil
}

func (s *Source) locksPath() string {
	if s.LocksPath != "" {
		return s.LocksPath
	}
	return procLocks
}

// listLocks lists the account lock files: regular files whose names match the runner's rule, at most maxLocks.
// key is the "major:minor:inode" /proc/locks prints for the file.
func (s *Source) listLocks() ([]lockFile, int, error) {
	d, err := safefs.OpenDir(s.LockDir)
	if err != nil {
		return nil, 0, dropReason(err)
	}
	defer d.Close()
	infos, err := d.Readdir(-1) // lstat of each entry; nothing is opened, so no lock is ever taken
	if err != nil {
		return nil, 0, fail(core.ErrReason(err))
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name() < infos[j].Name() })
	var out []lockFile
	over := 0
	for _, fi := range infos {
		m := lockName.FindStringSubmatch(fi.Name())
		if m == nil || !fi.Mode().IsRegular() {
			continue
		}
		key, ok := lockKeyOf(fi)
		if !ok {
			return nil, 0, fail(core.ReasonOther)
		}
		if len(out) >= maxLocks {
			over++
			continue
		}
		out = append(out, lockFile{tool: m[1], account: m[2], key: key})
	}
	return out, over, nil
}

func dropReason(err error) error {
	if se, ok := err.(*core.SourceError); ok {
		c := *se
		c.DropCache = true
		return &c
	}
	return fail(core.ReasonOther)
}

// heldLocks returns the set of files some process holds a flock on. A line has the form
//
//	1: FLOCK  ADVISORY  WRITE 968 00:19:1359 0 EOF
//
// with the device in hex (major:minor) and the inode in decimal; a process waiting for the lock has its own
// line, marked "->", which does not count as holding it.
func heldLocks(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fail(core.ErrReason(err))
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		return nil, fail(core.ErrReason(err))
	}
	return parseLocks(b), nil
}

func parseLocks(b []byte) map[string]bool {
	held := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || f[1] != "FLOCK" {
			continue // "->" lines have it in the third field; POSIX and OFD locks are not flocks
		}
		for _, t := range f[2:] {
			if k, ok := normKey(t); ok {
				held[k] = true
				break
			}
		}
	}
	return held
}

// normKey turns "00:19:1359" (hex device, decimal inode) into the decimal key.
func normKey(t string) (string, bool) {
	p := strings.Split(t, ":")
	if len(p) != 3 {
		return "", false
	}
	maj, e1 := strconv.ParseUint(p[0], 16, 32)
	min, e2 := strconv.ParseUint(p[1], 16, 32)
	ino, e3 := strconv.ParseUint(p[2], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return "", false
	}
	return fmt.Sprintf("%d:%d:%d", maj, min, ino), true
}

// logRow is the allow-listed view of a call-log line.
type logRow struct {
	TS       string   `json:"ts"`
	Tool     string   `json:"tool"`
	Account  string   `json:"account"`
	Duration *float64 `json:"duration_s"`
	Exit     *int     `json:"exit_code"`
}

// Exit codes the runner logs for calls that never ran a job (busy with --no-wait, wait timeout).
func notAJob(exit int) bool { return exit == 75 || exit == 124 }

// readLog consumes new complete lines of the call log from st.Off and folds them into st.Last. The first run
// (Off 0) reads at most the last firstReadMax bytes. A log shorter than Off was replaced: the offset moves to the
// end without replaying it. It returns the number of malformed lines.
func (s *Source) readLog(st *state, have map[string]bool) (int, error) {
	f, fi, err := safefs.OpenRegular(s.CallLog)
	if err != nil {
		if e, ok := err.(*core.SourceError); ok && e.Reason == core.ReasonNotFound {
			return 0, nil // no call has been logged yet
		}
		return 0, dropReason(err)
	}
	defer f.Close()
	size := fi.Size()
	skipFirst := false
	switch {
	case size < st.Off:
		st.Off = size
		return 0, nil
	case st.Off == 0 && size > firstReadMax:
		st.Off = size - firstReadMax
		skipFirst = true // lands inside a line
	}
	if _, err := f.Seek(st.Off, io.SeekStart); err != nil {
		return 0, fail(core.ReasonOther)
	}
	rd := bufio.NewReaderSize(io.LimitReader(f, size-st.Off), 64<<10)
	parse := 0
	for {
		line, err := readLine(rd)
		if err == io.EOF {
			break
		}
		if err != nil {
			return parse, fail(core.ErrReason(err))
		}
		if line.partial {
			break // the writer has not finished this line; take it next cycle
		}
		st.Off += int64(line.n)
		if skipFirst {
			skipFirst = false
			continue
		}
		if line.tooLong {
			parse++
			continue
		}
		if len(bytes.TrimSpace(line.b)) == 0 {
			continue
		}
		var row logRow
		if json.Unmarshal(line.b, &row) != nil {
			parse++
			continue
		}
		s.fold(st, have, row)
	}
	return parse, nil
}

func (s *Source) fold(st *state, have map[string]bool, row logRow) {
	if (row.Tool != "codex" && row.Tool != "agy") || !accountName.MatchString(row.Account) ||
		!have[row.Tool+"/"+row.Account] || row.Duration == nil || row.Exit == nil || notAJob(*row.Exit) ||
		*row.Duration < 0 {
		return
	}
	ts, err := time.Parse(time.RFC3339Nano, row.TS)
	if err != nil {
		return
	}
	end := float64(ts.UnixNano())/1e9 + *row.Duration
	key := row.Tool + "/" + row.Account
	if cur, ok := st.Last[key]; ok && cur.End > end {
		return // log order is call-start order, not end order: keep the latest end
	}
	st.Last[key] = last{End: end, Exit: *row.Exit}
}

type logLine struct {
	b       []byte
	n       int // bytes consumed including the newline
	partial bool
	tooLong bool
}

// readLine reads one line, keeping at most maxLine bytes of it.
func readLine(rd *bufio.Reader) (logLine, error) {
	var l logLine
	for {
		chunk, isPrefix, err := readSlice(rd)
		if err != nil && len(chunk) == 0 && l.n == 0 {
			return l, err
		}
		l.n += len(chunk)
		if len(l.b)+len(chunk) > maxLine {
			l.tooLong = true
		} else if !l.tooLong {
			l.b = append(l.b, chunk...)
		}
		if err != nil { // EOF without a newline
			l.partial = true
			return l, nil
		}
		if !isPrefix {
			l.b = bytes.TrimRight(l.b, "\r\n")
			if l.tooLong {
				l.b = nil
			}
			return l, nil
		}
	}
}

// readSlice returns the next piece of a line; isPrefix is true when the line continues.
func readSlice(rd *bufio.Reader) ([]byte, bool, error) {
	chunk, err := rd.ReadSlice('\n')
	switch err {
	case nil:
		return chunk, false, nil
	case bufio.ErrBufferFull:
		return chunk, true, nil
	}
	return chunk, false, err
}
