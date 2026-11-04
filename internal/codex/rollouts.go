package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/safefs"
)

const (
	readBuf   = 64 << 10 // line reader buffer; a line longer than this is judged by its first readBuf bytes
	maxLine   = 4 << 20  // a relevant line longer than this is skipped and counted as a parse error
	copyGrace = time.Hour
	scanFiles = 20      // first start: newest rollouts (by mtime) searched for the last quota reading
	scanTail  = 1 << 20 // ... reading at most this many bytes from the end of each
	walkDepth = 3       // sessions/YYYY/MM/DD
	modelStep = 1 << 20 // model search back from a file's offset: window step; a window spans two steps
	modelScan = 16 << 20
)

// RolloutsSource is `codex-rollouts`: token and request counters, quota windows and credits from
// <CODEX_HOME>/sessions/YYYY/MM/DD/rollout-*.jsonl.
//
// Counting rules (specs/sources.md, Codex; contract "Counter semantics"), as for claude-transcripts:
//   - the first collect for a profile starts every existing file at its end: no replay of history;
//   - files that appear later are read from the start, but lines older than the file's first sighting minus
//     one hour are skipped (history copied into a new file on fork or resume);
//   - lines past a stored offset are always counted, however old (an exporter outage loses nothing);
//   - a token_count event counts its last_token_usage only when its total_token_usage differs from the previous
//     total in that file (Codex repeats an event with an unchanged total).
//
// Quota is a gauge: per scope (limit_id), the newest token_count with rate_limits, by its own timestamp, across
// all files. The first collect searches the tails of the newest files for them, so an idle profile has readings
// at once.
type RolloutsSource struct {
	Host       string
	Alias      string
	Account    string
	Dir        string // real CODEX_HOME path (symlinks resolved at start)
	MaxModels  int
	StaleAfter time.Duration // a quota window without a reset time is served this long after its reading
	Interval   time.Duration

	mu sync.Mutex
	st *state // in-memory state, loaded from the persisted blob on the first collect
}

type fileState struct {
	Off int64  `json:"o"`
	Ino uint64 `json:"i"`
	// First: lines older than First - copyGrace are history, not new usage; 0 = no filter.
	First int64 `json:"f,omitempty"`
	// Model is the bounded label of the file's latest turn_context; "" = none seen yet.
	Model string `json:"m,omitempty"`
	// Total is the fingerprint of the file's latest total_token_usage, to skip repeated events.
	Total string `json:"t,omitempty"`
}

type usage struct {
	In        int64 `json:"in"`
	Out       int64 `json:"out"`
	CR        int64 `json:"cr"`
	CW        int64 `json:"cw"`
	Reasoning int64 `json:"rs"`
	Req       int64 `json:"req"`
}

type state struct {
	Init   bool                  `json:"init"`
	Files  map[string]*fileState `json:"files"`  // key: hashed path relative to sessions/
	Totals map[string]*usage     `json:"totals"` // key: model label (decided at count time)
	// Named holds the models that have their own series and when each was last used (see labelFor).
	Named       map[string]int64    `json:"named"`
	LastCollect int64               `json:"last_collect,omitempty"`
	Quotas      map[string]*reading `json:"quotas,omitempty"` // key: scope label; at most maxScopes
}

func (s *RolloutsSource) Name() string               { return SourceName }
func (s *RolloutsSource) Harness() string            { return Harness }
func (s *RolloutsSource) Profile() string            { return s.Alias }
func (s *RolloutsSource) MinInterval() time.Duration { return s.Interval }
func (s *RolloutsSource) Network() bool              { return false }

// tokenUsage is Codex's TokenUsage (codex-rs/protocol). input_tokens includes cached_input_tokens and
// cache_write_input_tokens; output_tokens includes reasoning_output_tokens (specs/sources.md, Codex).
type tokenUsage struct {
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_output_tokens"`
	Total      int64 `json:"total_tokens"`
}

func (u tokenUsage) fingerprint() string {
	return fmt.Sprintf("%d,%d,%d,%d,%d,%d", u.Input, u.Cached, u.CacheWrite, u.Output, u.Reasoning, u.Total)
}

// rolloutLine decodes only the structural fields this source uses. Every other field of a line (message text,
// cwd, instructions, tool output) is never decoded, so it cannot be retained.
type rolloutLine struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   *struct {
		Type  string `json:"type"`
		Model string `json:"model"` // turn_context
		Info  *struct {
			Total *tokenUsage `json:"total_token_usage"`
			Last  *tokenUsage `json:"last_token_usage"`
		} `json:"info"`
		RateLimits *rateLimits `json:"rate_limits"`
	} `json:"payload"`
}

// Lines carrying neither marker are skipped before any JSON parsing.
var (
	markTokenCount  = []byte(`"token_count"`)
	markTurnContext = []byte(`"turn_context"`)
)

func relevant(b []byte) bool {
	return bytes.Contains(b, markTokenCount) || bytes.Contains(b, markTurnContext)
}

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

type rollout struct {
	rel  string
	path string
	size int64
	ino  uint64
}

func (s *RolloutsSource) load(raw json.RawMessage) {
	if s.st != nil {
		return
	}
	st := &state{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, st) // unreadable plugin state = start over, no replay (first run rules)
	}
	if st.Files == nil {
		st.Files = map[string]*fileState{}
	}
	if st.Totals == nil {
		st.Totals = map[string]*usage{}
	}
	if st.Named == nil {
		st.Named = map[string]int64{}
	}
	if st.Quotas == nil {
		st.Quotas = map[string]*reading{}
	}
	s.st = st
}

func (s *RolloutsSource) Collect(ctx context.Context, now time.Time, raw json.RawMessage) (core.Result, json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load(raw)
	st := s.st
	res := core.Result{}

	if err := safefs.RealDir(s.Dir); err != nil {
		return res, nil, err
	}
	files, err := s.list()
	if err != nil {
		return res, nil, err // sessions/ missing, a link or unreadable: never zero values
	}

	parseErr := 0
	first := !st.Init
	// since: files that are new or replaced since the last collect count lines from that moment on.
	since := st.LastCollect
	if since == 0 {
		since = now.Unix()
	}
	// Every listed file is live, read or not: a read cut short by the timeout must keep the offsets of the files it
	// did not reach, or they would be re-read from 0 next time and counted twice.
	live := map[string]bool{}
	for _, f := range files {
		live[hashKey(f.rel)] = true
	}
	for _, f := range files {
		k := hashKey(f.rel)
		if first {
			st.Files[k] = &fileState{Off: f.size, Ino: f.ino}
			continue
		}
		fst := st.Files[k]
		if fst == nil {
			fst = &fileState{Ino: f.ino, First: since}
			st.Files[k] = fst
		}
		if fst.Ino != f.ino || f.size < fst.Off {
			// replaced or truncated: read again from the start, as a new file
			*fst = fileState{Ino: f.ino, First: since}
		}
		if f.size == fst.Off {
			continue
		}
		if ctx.Err() != nil {
			break // partial success: progress so far is kept, the rest is read next time
		}
		if fst.Model == "" && fst.Off > 0 {
			// started inside the file (first collect): its last turn_context lies before the offset
			done := s.findModel(ctx, f.path, fst)
			if ctx.Err() != nil {
				break // search cut short: count nothing under a guessed model, retry next time
			}
			if !done {
				continue // file unreadable now: count nothing under a guessed model, retry next time
			}
		}
		n, perr, _ := s.readRange(ctx, f.path, fst.Off, f.size, false, func(ln []byte) bool {
			return s.handle(ln, fst, true, now)
		})
		parseErr += perr
		fst.Off += n
	}
	if first {
		parseErr += s.scanForQuota(ctx, files, now)
	}
	for k := range st.Files {
		if !live[k] {
			delete(st.Files, k)
		}
	}
	st.Init = true
	st.LastCollect = now.Unix()

	if parseErr > 0 {
		res.Errors = map[string]int{core.ReasonParse: parseErr}
	}
	res.Samples = append(s.counterSamples(), s.quotaSamples(now)...)
	blob, err := json.Marshal(st)
	if err != nil {
		return res, nil, core.Fail(core.ReasonOther)
	}
	return res, blob, nil
}

// list walks sessions/ by directory handle: no link is followed at any level, and only regular rollout-*.jsonl
// files are returned, sorted by relative path.
func (s *RolloutsSource) list() ([]rollout, error) {
	root := filepath.Join(s.Dir, "sessions")
	rd, err := safefs.OpenDir(root)
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	var out []rollout
	var walk func(d *os.File, rel string, depth int)
	walk = func(d *os.File, rel string, depth int) {
		ents, err := d.ReadDir(-1)
		if err != nil {
			return
		}
		for _, e := range ents {
			name := e.Name()
			r := filepath.Join(rel, name)
			switch {
			case e.IsDir() && depth < walkDepth:
				if sd, err := safefs.OpenSubdir(d, name); err == nil {
					walk(sd, r, depth+1)
					sd.Close()
				}
			case e.Type().IsRegular() && strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl"):
				if size, ino, ok := safefs.StatAt(d, name); ok {
					out = append(out, rollout{r, filepath.Join(root, r), size, ino})
				}
			}
		}
	}
	walk(rd, "", 0)
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out, nil
}

// scanForQuota runs on the first collect only. It reads the tails of the newest files by mtime, up to the
// offsets just stored, so the profile has its last quota reading per scope, and each scanned file its current model and
// total, before anything new is written. Nothing is counted.
func (s *RolloutsSource) scanForQuota(ctx context.Context, files []rollout, now time.Time) int {
	type cand struct {
		f   rollout
		mod time.Time
	}
	var cs []cand
	for _, f := range files {
		if ctx.Err() != nil {
			return 0
		}
		fh, fi, err := safefs.OpenRegular(f.path)
		if err != nil {
			continue
		}
		fh.Close()
		cs = append(cs, cand{f, fi.ModTime()})
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].mod.After(cs[j].mod) })
	if len(cs) > scanFiles {
		cs = cs[:scanFiles]
	}
	parseErr := 0
	for _, c := range cs {
		if ctx.Err() != nil {
			break
		}
		fst := s.st.Files[hashKey(c.f.rel)]
		from := max(0, c.f.size-scanTail)
		_, perr, _ := s.readRange(ctx, c.f.path, from, c.f.size, from > 0, func(ln []byte) bool {
			return s.handle(ln, fst, false, now)
		})
		parseErr += perr
	}
	return parseErr
}

// findModel sets the model of a file that was first read from an offset inside it: the model of the last
// turn_context before that offset, searched backwards in windows of two steps that overlap by one step (a
// turn_context line up to modelStep long is never cut by a window edge), at most modelScan bytes back. If none is
// found the model is "unknown" until the file's next turn_context, so the search runs once per file. It reports
// false, leaving the model unset, when a read failed or was cut short: "unknown" only follows a completed search.
// Lines here are history: a malformed one is not counted as a parse error (windows overlap, it would count twice).
func (s *RolloutsSource) findModel(ctx context.Context, path string, fst *fileState) bool {
	for hi := fst.Off; hi > 0 && fst.Off-hi < modelScan; hi -= modelStep {
		lo := max(0, hi-modelStep)
		model := ""
		_, _, ok := s.readRange(ctx, path, lo, min(fst.Off, lo+2*modelStep), lo > 0, func(ln []byte) bool {
			var l rolloutLine
			if json.Unmarshal(ln, &l) != nil {
				return false
			}
			if l.Type == "turn_context" && l.Payload != nil {
				model = modelLabel(l.Payload.Model)
			}
			return true
		})
		if !ok || ctx.Err() != nil {
			return false
		}
		if model != "" {
			fst.Model = model
			return true
		}
	}
	fst.Model = "unknown"
	return true
}

// readRange reads the complete lines between from and to and passes the relevant ones to fn, which reports
// whether the line parsed. With partial set, from may be inside a line and that first line is dropped (the
// offset returned is then not meaningful). It returns the bytes consumed from `from` up to the end of the last
// complete line (an unterminated tail is left for the next collect), the parse error count, and whether the range was
// read to its end (false on an open, seek or read error, or when ctx ended the read). Memory is bounded: an irrelevant line is never buffered, and a relevant one only
// up to maxLine.
// openRollout opens a rollout for reading; a variable so a test can make a read fail after the file was listed.
var openRollout = safefs.OpenRegular

func (s *RolloutsSource) readRange(ctx context.Context, path string, from, to int64, partial bool, fn func([]byte) bool) (int64, int, bool) {
	f, _, err := openRollout(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	start := from
	if partial {
		start = from - 1 // the byte before from: if it ends a line, the line at from is whole and kept
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return 0, 0, false
	}
	r := bufio.NewReaderSize(io.LimitReader(f, to-start), readBuf)
	var pos, lineEnd int64 // bytes read; bytes up to the end of the last complete line
	parseErr := 0
	var buf []byte
	long, keep, tooLong := false, false, false
	skip := partial
	complete := true
	for n := 0; ; n++ {
		if n%1000 == 999 && ctx.Err() != nil {
			complete = false
			break
		}
		chunk, err := r.ReadSlice('\n')
		pos += int64(len(chunk))
		if errors.Is(err, bufio.ErrBufferFull) {
			if !long { // the chunk is the line's first readBuf bytes
				long, keep, tooLong = true, !skip && relevant(chunk), false
			}
			if keep && !tooLong {
				buf = append(buf, chunk...)
				if len(buf) > maxLine {
					tooLong, buf = true, nil
				}
			}
			continue
		}
		if err != nil { // EOF or read error
			complete = errors.Is(err, io.EOF)
			break
		}
		lineEnd = pos
		ln := chunk
		wasLong := long
		long = false
		if skip {
			skip, buf = false, nil
			continue
		}
		if wasLong {
			if !keep {
				continue
			}
			if tooLong || len(buf)+len(chunk) > maxLine {
				parseErr++
				buf = nil
				continue
			}
			ln, buf = append(buf, chunk...), nil
		} else if !relevant(ln) {
			continue
		}
		if !fn(ln) {
			parseErr++
		}
	}
	return lineEnd, parseErr, complete
}

// handle applies one relevant line to the file's state; count is false while scanning history on first start.
func (s *RolloutsSource) handle(ln []byte, fst *fileState, count bool, now time.Time) bool {
	var l rolloutLine
	if json.Unmarshal(ln, &l) != nil {
		return false
	}
	p := l.Payload
	if p == nil {
		return true
	}
	switch {
	case l.Type == "turn_context":
		fst.Model = modelLabel(p.Model)
	case l.Type == "event_msg" && p.Type == "token_count":
		ts, tsErr := time.Parse(time.RFC3339Nano, l.Timestamp)
		if p.RateLimits != nil && tsErr == nil {
			s.observe(p.RateLimits, ts)
		}
		if p.Info == nil || p.Info.Last == nil {
			return true
		}
		if p.Info.Total != nil {
			fp := p.Info.Total.fingerprint()
			if fp == fst.Total {
				return true // the same event written again
			}
			fst.Total = fp
		}
		if !count {
			return true
		}
		if fst.First > 0 && tsErr == nil && ts.Before(time.Unix(fst.First, 0).Add(-copyGrace)) {
			return true // history copied into a file that appeared after start
		}
		s.add(fst.Model, *p.Info.Last, now)
	}
	return true
}

func pos(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// add counts one model call. Codex nests cached and cache-write input inside input, and reasoning inside output;
// the contract's types are disjoint, so the nested parts are subtracted.
func (s *RolloutsSource) add(model string, u tokenUsage, now time.Time) {
	if model == "" {
		model = "unknown"
	}
	m := s.labelFor(model, now)
	t := s.st.Totals[m]
	if t == nil {
		t = &usage{}
		s.st.Totals[m] = t
	}
	t.In += pos(u.Input - u.Cached - u.CacheWrite)
	t.CR += pos(u.Cached)
	t.CW += pos(u.CacheWrite)
	t.Out += pos(u.Output - u.Reasoning)
	t.Reasoning += pos(u.Reasoning)
	t.Req++
}

// staleModel is how long a named model must go unused before a new model may take its slot.
const staleModel = 24 * time.Hour

// labelFor returns the series a call of this model counts into. A model keeps its own series while it is among
// the MaxModels named models of the profile; a new model takes a free slot, or the slot of a model unused for a
// day (that model's series ends; its total is not moved), else it counts into model="other". Totals never move
// between series, so no counter goes backwards (contract cardinality rule 3). Same rule as claude-transcripts.
func (s *RolloutsSource) labelFor(model string, now time.Time) string {
	if model == "other" {
		return model
	}
	named := s.st.Named
	if _, ok := named[model]; ok {
		named[model] = now.Unix()
		return model
	}
	limit := s.MaxModels
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
		delete(s.st.Totals, victim)
	}
	named[model] = now.Unix()
	return model
}

// counterSamples emits the totals as they were counted.
func (s *RolloutsSource) counterSamples() []core.Sample {
	models := make([]string, 0, len(s.st.Totals))
	for m := range s.st.Totals {
		models = append(models, m)
	}
	sort.Strings(models)
	var out []core.Sample
	for _, m := range models {
		t := s.st.Totals[m]
		model := core.Label{Name: "model", Value: m}
		for _, tv := range []struct {
			typ string
			v   int64
		}{{"input", t.In}, {"output", t.Out}, {"cache_read", t.CR}, {"cache_write", t.CW}, {"reasoning", t.Reasoning}} {
			out = append(out, core.Sample{Name: "agent_tokens_total",
				Labels: s.labels(model, core.Label{Name: "type", Value: tv.typ}), Value: float64(tv.v)})
		}
		out = append(out, core.Sample{Name: "agent_requests_total",
			Labels: s.labels(model, core.Label{Name: "outcome", Value: "ok"}), Value: float64(t.Req)})
	}
	return out
}
