package claude

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

const (
	maxLine        = 16 << 20 // longer lines are skipped and counted as parse errors
	seenCap        = 20000    // message ids kept in memory for dedupe
	seenPersistCap = 2000     // newest ids persisted (covers a stream that straddles a restart)
	copyGrace      = time.Hour
)

// TranscriptsSource is `claude-transcripts`: token and request counters from <dir>/projects/**/*.jsonl.
//
// Counting rules (specs/sources.md, contract "Counter semantics"):
//   - the first collect for a profile starts every existing file at its end: no replay of history;
//   - files that appear later are read from the start, but lines older than the file's first sighting minus
//     one hour are skipped (history copied into a new file on resume or fork);
//   - lines past a stored offset are always counted, however old (an exporter outage loses nothing);
//   - a message id seen again adds only the positive difference per field (streamed duplicates).
type TranscriptsSource struct {
	P        *Profile
	Interval time.Duration

	mu sync.Mutex
	st *tState // in-memory state, loaded from the persisted blob on the first collect
}

type fileState struct {
	Off   int64  `json:"o"`
	Ino   uint64 `json:"i"`
	First int64  `json:"f,omitempty"` // lines older than First - copyGrace are history, not new usage; 0 = no filter
}

type usage struct {
	In  int64 `json:"in"`
	Out int64 `json:"out"`
	CR  int64 `json:"cr"`
	CW  int64 `json:"cw"`
}

type seenMsg struct {
	U   usage `json:"u"`
	Seq int64 `json:"s"`
}

type total struct {
	usage
	Req int64 `json:"req"`
}

type tState struct {
	Init   bool                  `json:"init"`
	Files  map[string]*fileState `json:"files"` // key: hashed relative path
	Seen   map[string]*seenMsg   `json:"seen"`  // key: hashed message id
	Seq    int64                 `json:"seq"`
	Totals map[string]*total     `json:"totals"` // key: account \x00 model label (decided at count time)
	// Named holds, per account, the models that have their own series and when each was last used. The label of
	// a message is decided when it is counted and never changes afterwards, so no total moves between series.
	Named map[string]map[string]int64 `json:"named"`
	// LastCollect is the end of the previous collect. A file first seen after it (or replaced since) is new
	// unless its lines predate it: lines written during an exporter outage still count.
	LastCollect int64 `json:"last_collect,omitempty"`
}

func (s *TranscriptsSource) Name() string               { return "claude-transcripts" }
func (s *TranscriptsSource) Harness() string            { return Harness }
func (s *TranscriptsSource) Profile() string            { return s.P.Alias }
func (s *TranscriptsSource) MinInterval() time.Duration { return s.Interval }
func (s *TranscriptsSource) Network() bool              { return false }

func hashKey(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

var modelOK = regexp.MustCompile(`^[a-z0-9._:/@\[\]-]{1,64}$`)

// modelLabel bounds the untrusted model string: lowercase, safe charset, ≤ 64 bytes, else "other".
func modelLabel(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if m == "" {
		return "unknown"
	}
	if !modelOK.MatchString(m) {
		return "other"
	}
	return m
}

type line struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			In  int64 `json:"input_tokens"`
			Out int64 `json:"output_tokens"`
			CR  int64 `json:"cache_read_input_tokens"`
			CW  int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

var assistantMarker = []byte(`"assistant"`)

func (s *TranscriptsSource) Collect(ctx context.Context, now time.Time, raw json.RawMessage) (core.Result, json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == nil {
		st := &tState{}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, st) // unreadable plugin state = start over, no replay (first run rules)
		}
		if st.Files == nil {
			st.Files = map[string]*fileState{}
		}
		if st.Seen == nil {
			st.Seen = map[string]*seenMsg{}
		}
		if st.Named == nil {
			st.Named = map[string]map[string]int64{}
		}
		if st.Totals == nil {
			st.Totals = map[string]*total{}
		}
		s.st = st
	}
	st := s.st
	res := core.Result{}
	parseErr := 0

	if err := realDir(s.P.Dir); err != nil {
		return res, nil, err
	}
	root := filepath.Join(s.P.Dir, "projects")
	if fi, err := os.Lstat(root); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return res, nil, core.Fail(core.ReasonPermission) // never follow a user-controlled link out of the profile
	}
	type found struct {
		rel  string
		path string
		size int64
		ino  uint64
	}
	var files []found
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil // an unreadable sub-tree is skipped, not fatal
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 || !strings.HasSuffix(d.Name(), ".jsonl") || !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		var ino uint64
		if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
			ino = sys.Ino
		}
		rel, _ := filepath.Rel(root, path)
		files = append(files, found{rel, path, fi.Size(), ino})
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, os.ErrNotExist) {
		return res, nil, core.Fail(core.ErrReason(walkErr))
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })

	// since: files that are new or replaced since the last collect count lines from that moment on.
	since := st.LastCollect
	if since == 0 {
		since = now.Unix()
	}
	live := map[string]bool{}
	account, _ := s.P.Account()
	for _, f := range files {
		k := hashKey(f.rel)
		live[k] = true
		fst := st.Files[k]
		if !st.Init {
			st.Files[k] = &fileState{Off: f.size, Ino: f.ino}
			continue
		}
		if fst == nil {
			fst = &fileState{Ino: f.ino, First: since}
			st.Files[k] = fst
		}
		if fst.Ino != f.ino || f.size < fst.Off {
			// replaced or truncated: read again from the start; dedupe stops double counting.
			fst.Off, fst.Ino, fst.First = 0, f.ino, since
		}
		if f.size == fst.Off {
			continue
		}
		if ctx.Err() != nil {
			break // partial success: progress so far is kept, the rest is read next time
		}
		n, perr := s.readFile(ctx, f.path, fst, account, now)
		parseErr += perr
		fst.Off += n
	}
	for k := range st.Files {
		if !live[k] && st.Init {
			delete(st.Files, k)
		}
	}
	st.Init = true
	st.LastCollect = now.Unix()
	s.evict()

	if parseErr > 0 {
		res.Errors = map[string]int{core.ReasonParse: parseErr}
	}
	res.Samples = s.samples()
	blob, err := json.Marshal(s.persistView())
	if err != nil {
		return res, nil, core.Fail(core.ReasonOther)
	}
	return res, blob, nil
}

// readFile counts complete lines from fst.Off and returns the bytes consumed (up to the end of the last complete
// line) and the parse error count. An unterminated tail is left for the next collect.
func (s *TranscriptsSource) readFile(ctx context.Context, path string, fst *fileState, account string, now time.Time) (int64, int) {
	f, _, err := openRegular(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	if _, err := f.Seek(fst.Off, io.SeekStart); err != nil {
		return 0, 0
	}
	r := bufio.NewReaderSize(f, 64<<10)
	var pos, lineEnd int64 // bytes read; bytes up to the end of the last complete line
	parseErr := 0
	var buf []byte
	oversized := false
	for n := 0; ; n++ {
		if n%1000 == 999 && ctx.Err() != nil {
			return lineEnd, parseErr
		}
		chunk, err := r.ReadSlice('\n')
		pos += int64(len(chunk))
		if errors.Is(err, bufio.ErrBufferFull) {
			if !oversized {
				buf = append(buf, chunk...)
				if len(buf) > maxLine {
					oversized, buf = true, nil
				}
			}
			continue
		}
		if err != nil { // EOF or read error
			return lineEnd, parseErr
		}
		lineEnd = pos
		if oversized {
			oversized = false
			parseErr++
			continue
		}
		ln := chunk
		if len(buf) > 0 {
			ln = append(buf, chunk...)
			buf = nil
		}
		if !bytes.Contains(ln, assistantMarker) {
			continue
		}
		if !s.count(ln, fst, account, now) {
			parseErr++
		}
	}
}

func (s *TranscriptsSource) count(ln []byte, fst *fileState, account string, now time.Time) bool {
	var l line
	if json.Unmarshal(ln, &l) != nil {
		return false
	}
	if l.Type != "assistant" || l.Message == nil || l.Message.Usage == nil || l.Message.ID == "" ||
		l.Message.Model == "<synthetic>" {
		return true
	}
	if fst.First > 0 {
		ts, err := time.Parse(time.RFC3339Nano, l.Timestamp)
		if err == nil && ts.Before(time.Unix(fst.First, 0).Add(-copyGrace)) {
			return true // history copied into a file that appeared after start
		}
	}
	st := s.st
	u := usage{l.Message.Usage.In, l.Message.Usage.Out, l.Message.Usage.CR, l.Message.Usage.CW}
	key := hashKey(l.Message.ID)
	tk := account + "\x00" + s.labelFor(account, modelLabel(l.Message.Model), now)
	t := st.Totals[tk]
	if t == nil {
		t = &total{}
		st.Totals[tk] = t
	}
	st.Seq++
	if prev, ok := st.Seen[key]; ok {
		t.In += pos(u.In - prev.U.In)
		t.Out += pos(u.Out - prev.U.Out)
		t.CR += pos(u.CR - prev.U.CR)
		t.CW += pos(u.CW - prev.U.CW)
		prev.U = usage{max(u.In, prev.U.In), max(u.Out, prev.U.Out), max(u.CR, prev.U.CR), max(u.CW, prev.U.CW)}
		prev.Seq = st.Seq
		return true
	}
	st.Seen[key] = &seenMsg{U: u, Seq: st.Seq}
	t.In += u.In
	t.Out += u.Out
	t.CR += u.CR
	t.CW += u.CW
	t.Req++
	return true
}

func pos(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// evict keeps the seen set bounded by count (oldest sightings dropped).
func (s *TranscriptsSource) evict() {
	if len(s.st.Seen) <= seenCap {
		return
	}
	s.st.Seen = newest(s.st.Seen, seenCap)
}

func newest(m map[string]*seenMsg, n int) map[string]*seenMsg {
	type kv struct {
		k   string
		seq int64
	}
	all := make([]kv, 0, len(m))
	for k, v := range m {
		all = append(all, kv{k, v.Seq})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seq > all[j].seq })
	out := make(map[string]*seenMsg, n)
	for i := 0; i < len(all) && i < n; i++ {
		out[all[i].k] = m[all[i].k]
	}
	return out
}

// persistView is the state written to disk: everything but the seen set trimmed to its newest entries.
func (s *TranscriptsSource) persistView() *tState {
	v := *s.st
	if len(v.Seen) > seenPersistCap {
		v.Seen = newest(v.Seen, seenPersistCap)
	}
	return &v
}

// staleModel is how long a named model must go unused before a new model may take its slot.
const staleModel = 24 * time.Hour

// labelFor returns the series a message of this model counts into. A model keeps its own series while it is
// among the max_models named models of the account; a new model takes a free slot, or the slot of a model unused
// for a day (that model's series ends; its total is not moved), else it counts into model="other". Totals never
// move between series, so no counter goes backwards (contract cardinality rule 3: top N by recent use).
func (s *TranscriptsSource) labelFor(account, model string, now time.Time) string {
	if model == "other" {
		return model
	}
	named := s.st.Named[account]
	if named == nil {
		named = map[string]int64{}
		s.st.Named[account] = named
	}
	if _, ok := named[model]; ok {
		named[model] = now.Unix()
		return model
	}
	limit := s.P.MaxModels
	if limit <= 0 {
		limit = 20
	}
	if len(named) >= limit {
		victim, oldest := "", now.Unix()
		for m, last := range named {
			if last < oldest || (last == oldest && m < victim) {
				victim, oldest = m, last
			}
		}
		if now.Sub(time.Unix(oldest, 0)) < staleModel {
			return "other"
		}
		delete(named, victim)
		delete(s.st.Totals, account+"\x00"+victim)
	}
	named[model] = now.Unix()
	return model
}

// samples emits the totals as they were counted.
func (s *TranscriptsSource) samples() []core.Sample {
	keys := make([]string, 0, len(s.st.Totals))
	for k := range s.st.Totals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []core.Sample
	for _, k := range keys {
		acct, m, _ := strings.Cut(k, "\x00")
		f := s.st.Totals[k]
		base := func(extra ...core.Label) []core.Label {
			return s.P.labels(acct, append([]core.Label{{Name: "provider", Value: Provider}, {Name: "model", Value: m}}, extra...)...)
		}
		for _, tv := range []struct {
			typ string
			v   int64
		}{{"input", f.In}, {"output", f.Out}, {"cache_read", f.CR}, {"cache_write", f.CW}} {
			out = append(out, core.Sample{Name: "agent_tokens_total", Labels: base(core.Label{Name: "type", Value: tv.typ}), Value: float64(tv.v)})
		}
		out = append(out, core.Sample{Name: "agent_requests_total", Labels: base(core.Label{Name: "outcome", Value: "ok"}), Value: float64(f.Req)})
	}
	return out
}
