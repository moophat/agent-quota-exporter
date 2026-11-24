// Package jobs reads which agent jobs are running now from the files a task runner leaves on the host
// (`job-locks`, specs/sources.md, "Jobs").
//
// The convention it reads: a task runner holds one flocked <tool>-<account>.lock in <lock_dir> per running job and
// appends one JSON line per call to a call log. Supported tools are codex and agy. The source never takes the
// flock (it reads /proc/locks, which lists who holds which lock), so it cannot make a runner wait or fail. From the
// call log it decodes only ts, tool, account, duration_s and exit_code, and keeps the end time and exit code of the
// last finished call per account.
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
	"syscall"
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

	maxLocks     = 100     // lock files exported per cycle; the rest count as source errors
	maxHistory   = 1000    // accounts whose last job is remembered, exported or not
	dirBatch     = 256     // directory entries read at a time
	firstReadMax = 8 << 20 // bytes of call log read on the first run and per cycle
	maxLine      = 64 << 10
	procLocks    = "/proc/locks"
)

// maxLockTable bounds the /proc/locks read. A table larger than this is refused, never read as if it were whole.
var maxLockTable int64 = 4 << 20

// lockName is the lock file name rule: a supported tool prefix and a bounded account name.
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
	Off     int64           `json:"off"`           // call-log bytes consumed
	Dev     uint64          `json:"dev,omitempty"` // device and inode of the log being read: another file is a new log
	Ino     uint64          `json:"ino,omitempty"`
	Discard bool            `json:"discard,omitempty"` // inside an over-long line: skip to its newline
	Last    map[string]last `json:"last"`
}

// fail marks a read the source cannot vouch for: the series vanish and the source reports down.
func fail(reason string) error { return &core.SourceError{Reason: reason, DropCache: true} }

type lockFile struct{ name, tool, account, key string }

func (s *Source) Collect(_ context.Context, _ time.Time, raw json.RawMessage) (core.Result, json.RawMessage, error) {
	res := core.Result{}
	st := state{Last: map[string]last{}}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &st)
		if st.Last == nil {
			st.Last = map[string]last{}
		}
	}
	eligible, total, err := s.listLocks()
	if err != nil {
		return res, nil, err
	}
	if total > maxLocks {
		res.Errors = map[string]int{core.ReasonOther: total - maxLocks}
	}
	held, err := heldLocks(s.locksPath())
	if err != nil {
		return res, nil, err
	}
	// History is kept for every eligible account, exported or not; it goes only when the lock file does.
	have := map[string]bool{}
	for _, l := range eligible {
		have[l.tool+"/"+l.account] = true
	}
	for k := range st.Last {
		if !have[k] {
			delete(st.Last, k)
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
	shown := eligible
	if len(shown) > maxLocks {
		shown = shown[:maxLocks]
	}
	for _, l := range shown {
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

// refusedFS are filesystems where the device in a file's stat differs from the one /proc/locks prints, or where
// flock is not listed as FLOCK, so a held lock could never be matched and every job would read "not running".
// Magic numbers from statfs(2): btrfs, overlayfs, NFS.
var refusedFS = map[int64]bool{0x9123683e: true, 0x794c7630: true, 0x6969: true}

// fsType returns the statfs type of an open directory. Replaceable in tests.
var fsType = func(d *os.File) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Fstatfs(int(d.Fd()), &st); err != nil {
		return 0, err
	}
	return int64(st.Type), nil
}

// listLocks lists the eligible account lock files: regular files whose names match lockName, smallest
// names first, at most maxHistory. total counts every matching file. The directory is read in batches and only
// matches are kept (trimmed to a bound as it grows), so a huge directory costs time, not memory.
// key is the "major:minor:inode" /proc/locks prints for the file.
func (s *Source) listLocks() ([]lockFile, int, error) {
	d, err := safefs.OpenDir(s.LockDir)
	if err != nil {
		return nil, 0, dropReason(err)
	}
	defer d.Close()
	if t, err := fsType(d); err != nil || refusedFS[t] {
		return nil, 0, fail(core.ReasonOther)
	}
	var out []lockFile
	total := 0
	trim := func(limit int) {
		sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
		if len(out) > limit {
			out = out[:limit]
		}
	}
	for {
		infos, err := d.Readdir(dirBatch) // lstat of each entry; nothing is opened, so no lock is ever taken
		for _, fi := range infos {
			m := lockName.FindStringSubmatch(fi.Name())
			if m == nil || !fi.Mode().IsRegular() {
				continue
			}
			key, ok := lockKeyOf(fi)
			if !ok {
				return nil, 0, fail(core.ReasonOther)
			}
			total++
			out = append(out, lockFile{name: fi.Name(), tool: m[1], account: m[2], key: key})
			if len(out) >= 2*maxHistory {
				trim(maxHistory)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, fail(core.ErrReason(err))
		}
	}
	trim(maxHistory)
	return out, total, nil
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
	b, err := io.ReadAll(io.LimitReader(f, maxLockTable+1))
	if err != nil {
		return nil, fail(core.ErrReason(err))
	}
	if int64(len(b)) > maxLockTable {
		return nil, fail(core.ReasonOther) // a table cut short would turn held locks into "not running"
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

// Exit codes that mean a call never ran a job: 75 (EX_TEMPFAIL, the account was busy) and 124 (gave up waiting,
// the timeout(1) convention).
func notAJob(exit int) bool { return exit == 75 || exit == 124 }

// readLog consumes new complete lines of the call log from st.Off and folds them into st.Last, at most
// firstReadMax bytes per cycle. A log that is another file (device or inode changed) or shorter than the offset
// was replaced or rotated: it is read from its start like a first run, bounded by the same tail cap. A line over
// maxLine is counted once as malformed and skipped up to its newline, also when it is still being written. It
// returns the number of malformed lines.
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
	dev, ino := fileID(fi)
	if dev != st.Dev || ino != st.Ino || size < st.Off {
		st.Dev, st.Ino, st.Off, st.Discard = dev, ino, 0, false
	}
	if st.Off == 0 && size > firstReadMax {
		st.Off = size - firstReadMax
		// The cut lands inside a line unless the byte before it ends one.
		var prev [1]byte
		if _, err := f.ReadAt(prev[:], st.Off-1); err != nil {
			return 0, fail(core.ErrReason(err))
		}
		st.Discard = prev[0] != '\n'
	}
	if _, err := f.Seek(st.Off, io.SeekStart); err != nil {
		return 0, fail(core.ReasonOther)
	}
	rd := bufio.NewReaderSize(io.LimitReader(f, min(size-st.Off, firstReadMax)), 64<<10)
	parse := 0
	for {
		if st.Discard {
			n, found, err := skipLine(rd)
			if err != nil {
				return parse, fail(core.ErrReason(err))
			}
			st.Off += int64(n)
			if !found {
				break // still inside the long line; carry on next cycle
			}
			st.Discard = false
			continue
		}
		line, err := readLine(rd)
		if err == io.EOF {
			break
		}
		if err != nil {
			return parse, fail(core.ErrReason(err))
		}
		if line.partial {
			if line.n > maxLine { // an over-long unfinished line: count it once and skip the rest as it arrives
				st.Off += int64(line.n)
				st.Discard = true
				parse++
			}
			break // otherwise the writer has not finished this line; take it next cycle
		}
		st.Off += int64(line.n)
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

// skipLine consumes bytes up to and including the next newline without keeping them. found is false when the
// input ended first.
func skipLine(rd *bufio.Reader) (n int, found bool, err error) {
	for {
		chunk, err := rd.ReadSlice('\n')
		n += len(chunk)
		switch err {
		case nil:
			return n, true, nil
		case bufio.ErrBufferFull:
		case io.EOF:
			return n, false, nil
		default:
			return n, false, err
		}
	}
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
