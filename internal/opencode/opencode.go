// Package opencode reads an opencode database read-only (`opencode-db`): tokens, cost and errors per message.
//
// What is read (specs/sources.md, "opencode"): the `message` table only, through sqlro's allow-list: the row's
// rowid, its `id`, and scalars of `message.data` at the paths in allow. Nothing else of the JSON leaves SQLite,
// and no other table is listed, so `part` (message text), `session`, `account`, `control_account`, `credential`,
// `session_share` and the rest are refused before any SQL is built.
//
// Counting rules:
//   - opencode inserts an assistant message when the model call starts and updates the same row (same rowid) as
//     it streams and when it ends. A message is complete when `time.completed` is set or `error.name` is set (an
//     error ends the call; opencode writes both in its final update). Only complete assistant messages count,
//     each exactly once, with the values of that complete row.
//   - New rows are read by a rowid cursor. An incomplete assistant row goes into a persisted pending set and is
//     re-read by id every cycle until it completes, then counted. The pending set is capped (oldest dropped) and a
//     row pending for longer than a day (its writer died) is dropped; both drops are counted as source errors.
//   - The first collect for a profile starts the cursor at the current last rowid: history, including calls in
//     flight at that moment, is not counted. Later rows are always counted, however old, so an exporter outage
//     loses nothing. If the last rowid falls below the cursor (the database was replaced or vacuumed), the cursor
//     moves back to it, again without replay.
//   - A WAL database that no writer has open is read immutable by sqlro (no -wal or -shm is created), so the final
//     rows a closing opencode checkpointed are still counted. sqlro.ErrIdle now means only an odd state with one
//     sidecar missing: the persisted counters are served for that cycle.
package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/sqlro"
)

const (
	// Harness is the contract label value for this plugin.
	Harness = "opencode"
	// SourceName is the contract `source` value.
	SourceName = "opencode-db"

	dbFile        = "opencode.db"
	table         = "message"
	pageSize      = 2000           // rows per After call; a cycle reads pages until it catches up
	keysPerCall   = 500            // ids per ByKeys call (sqlro's bound)
	pendingCap    = 2000           // incomplete assistant messages tracked at once
	pendingMaxAge = 24 * time.Hour // an assistant message still incomplete after this is abandoned
	staleModel    = 24 * time.Hour // a named model unused this long may give its slot to a new one
	maxIDLen      = 120            // sqlro cuts text at 128 characters; a longer id could not be found again
)

// Columns of every query, in this order. rowid is implicit in sqlro's allow-list.
var cols = []sqlro.Expr{
	sqlro.Col("rowid"),
	sqlro.Col("id"),
	sqlro.JSON{Col: "data", Path: "$.role"},
	sqlro.JSON{Col: "data", Path: "$.providerID"},
	sqlro.JSON{Col: "data", Path: "$.modelID"},
	sqlro.JSON{Col: "data", Path: "$.cost"},
	sqlro.JSON{Col: "data", Path: "$.tokens.input"},
	sqlro.JSON{Col: "data", Path: "$.tokens.output"},
	sqlro.JSON{Col: "data", Path: "$.tokens.reasoning"},
	sqlro.JSON{Col: "data", Path: "$.tokens.cache.read"},
	sqlro.JSON{Col: "data", Path: "$.tokens.cache.write"},
	sqlro.JSON{Col: "data", Path: "$.time.completed"},
	sqlro.JSON{Col: "data", Path: "$.error.name"},
	sqlro.JSON{Col: "data", Path: "$.error.data.statusCode"},
}

const (
	cRowid = iota
	cID
	cRole
	cProvider
	cModel
	cCost
	cIn
	cOut
	cReasoning
	cCacheRead
	cCacheWrite
	cCompleted
	cErrName
	cErrStatus
)

// allow is everything this plugin may read: the message id and the listed scalars of message.data. message.data
// itself is never listed as a bare column (it is the whole JSON), and no other table is listed.
var allow = sqlro.Allow{table: cols[1:]}

// Build returns the sources for one resolved opencode profile; r.Dir is the data dir holding opencode.db.
func Build(cfg *config.Config, r config.Resolved, host string) []core.Source {
	account := r.Account
	if account == "" {
		account = r.Alias
	}
	return []core.Source{&DBSource{Host: host, Alias: r.Alias, Account: account,
		Path:      filepath.Join(r.Dir, dbFile),
		MaxModels: cfg.Metrics.MaxModelsPerProfile,
		Interval:  cfg.SourceInterval(SourceName, cfg.Defaults.UsageInterval.D())}}
}

// DBSource is `opencode-db` for one profile (one opencode data dir).
type DBSource struct {
	Host, Alias, Account string
	Path                 string // opencode.db
	MaxModels            int    // (provider, model) series per profile; the rest count into model="other"
	Interval             time.Duration

	mu sync.Mutex
	st *dbState // in-memory state, loaded from the persisted blob on the first collect
}

type pend struct {
	Row  int64 `json:"r"` // rowid when first seen; orders the cap's eviction
	Seen int64 `json:"s"` // Unix seconds of the first sighting
}

type total struct {
	In        int64            `json:"in"`
	Out       int64            `json:"out"`
	Reasoning int64            `json:"rs"`
	CacheRead int64            `json:"cr"`
	CacheW    int64            `json:"cw"`
	OK        int64            `json:"ok"`
	Err       int64            `json:"err"`
	Codes     map[string]int64 `json:"codes,omitempty"`
	Cost      float64          `json:"cost,omitempty"`
}

type dbState struct {
	Init    bool             `json:"init"`
	Cursor  int64            `json:"cursor"`            // last rowid read
	Ino     uint64           `json:"ino,omitempty"`     // inode of the database file; a change means it was replaced
	Pending map[string]*pend `json:"pending,omitempty"` // key: message id
	// Totals and Named are keyed by provider \x00 model label, decided when a message is counted and never
	// changed afterwards, so no total moves between series.
	Totals map[string]*total `json:"totals"`
	Named  map[string]int64  `json:"named"` // (provider, model) pairs with their own series → last use
}

func (s *DBSource) Name() string               { return SourceName }
func (s *DBSource) Harness() string            { return Harness }
func (s *DBSource) Profile() string            { return s.Alias }
func (s *DBSource) MinInterval() time.Duration { return s.Interval }
func (s *DBSource) Network() bool              { return false }

func (s *DBSource) load(raw json.RawMessage) {
	if s.st != nil {
		return
	}
	st := &dbState{}
	if len(raw) > 0 && json.Unmarshal(raw, st) != nil {
		st = &dbState{} // unreadable plugin state = start over, without replay
	}
	if st.Pending == nil {
		st.Pending = map[string]*pend{}
	}
	if st.Totals == nil {
		st.Totals = map[string]*total{}
	}
	if st.Named == nil {
		st.Named = map[string]int64{}
	}
	s.st = st
}

// Collect opens the database, reads what is new, closes it, and returns every counter.
func (s *DBSource) Collect(ctx context.Context, now time.Time, raw json.RawMessage) (core.Result, json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load(raw)

	db, err := sqlro.Open(s.Path, allow)
	if errors.Is(err, sqlro.ErrIdle) {
		return core.Result{Samples: s.samples()}, nil, nil // nothing can have changed; state unchanged
	}
	if err != nil {
		return core.Result{}, nil, err
	}
	errs := map[string]int{}
	err = s.read(ctx, db, fileIno(s.Path), now, errs)
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	res := core.Result{Samples: s.samples()}
	if len(errs) > 0 {
		res.Errors = errs
	}
	blob, merr := json.Marshal(s.st)
	if merr != nil {
		return res, nil, core.Fail(core.ReasonOther)
	}
	// Progress made before a failure is kept: every row applied was read whole.
	return res, blob, err
}

// fileIno is the database file's inode (0 if unknown). sqlro has already refused a path with a link on it.
func fileIno(path string) uint64 {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

func (s *DBSource) read(ctx context.Context, db *sqlro.DB, ino uint64, now time.Time, errs map[string]int) error {
	st := s.st
	last, err := db.MaxRowid(table)
	if err != nil {
		return err
	}
	if !st.Init {
		st.Init, st.Cursor, st.Ino = true, last, ino
		return nil
	}
	if ino != 0 && st.Ino != 0 && ino != st.Ino {
		// A different file: the database was replaced. No replay, and the pending ids belong to the old database, so
		// a replacement row reusing one of them must not be counted as its completion.
		st.Cursor = last
		st.Pending = map[string]*pend{}
	} else if last < st.Cursor {
		// Same file, newest rows deleted (a session removed): move the cursor back without replay, but keep the
		// pending ids. Messages still in flight in other sessions complete later; deleted ones drop out on re-read.
		st.Cursor = last
	}
	if ino != 0 {
		st.Ino = ino
	}
	for {
		rows, err := db.After(table, "rowid", st.Cursor, cols, pageSize)
		if err != nil {
			return err
		}
		for _, r := range rows {
			s.apply(r, now, errs)
			if id, ok := r[cRowid].(int64); ok && id > st.Cursor {
				st.Cursor = id
			}
		}
		if len(rows) < pageSize || ctx.Err() != nil {
			break
		}
	}

	s.capPending(errs)
	ids := make([]string, 0, len(st.Pending))
	for id := range st.Pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for i := 0; i < len(ids) && ctx.Err() == nil; i += keysPerCall {
		chunk := ids[i:min(i+keysPerCall, len(ids))]
		rows, err := db.ByKeys(table, "id", chunk, cols)
		if err != nil {
			return err
		}
		found := make(map[string]bool, len(rows))
		for _, r := range rows {
			found[str(r[cID])] = true
			s.apply(r, now, errs)
		}
		for _, id := range chunk {
			if !found[id] {
				delete(st.Pending, id) // the message (or its session) was deleted: nothing left to count
			}
		}
	}
	for id, p := range st.Pending {
		if now.Sub(time.Unix(p.Seen, 0)) > pendingMaxAge {
			delete(st.Pending, id)
			errs[core.ReasonOther]++
		}
	}
	return nil
}

// apply counts a complete assistant row once, tracks an incomplete one as pending, and ignores other roles.
func (s *DBSource) apply(r []any, now time.Time, errs map[string]int) {
	st := s.st
	id := str(r[cID])
	role, ok := r[cRole].(string)
	if !ok {
		delete(st.Pending, id)
		errs[core.ReasonParse]++ // data is not JSON or has no role
		return
	}
	if role != "assistant" {
		delete(st.Pending, id)
		return
	}
	errName := str(r[cErrName])
	if r[cCompleted] == nil && errName == "" {
		if id == "" || len(id) > maxIDLen {
			errs[core.ReasonParse]++
			return
		}
		if _, ok := st.Pending[id]; !ok {
			row, _ := r[cRowid].(int64)
			st.Pending[id] = &pend{Row: row, Seen: now.Unix()}
		}
		return
	}
	delete(st.Pending, id)

	provider := providerLabel(str(r[cProvider]))
	key := provider + "\x00" + s.labelFor(provider, modelLabel(str(r[cModel])), now)
	t := st.Totals[key]
	if t == nil {
		t = &total{}
		st.Totals[key] = t
	}
	t.In += count(r[cIn])
	t.Out += count(r[cOut])
	t.Reasoning += count(r[cReasoning])
	t.CacheRead += count(r[cCacheRead])
	t.CacheW += count(r[cCacheWrite])
	if c, ok := num(r[cCost]); ok && c > 0 {
		t.Cost += c
	}
	if errName == "" {
		t.OK++
		return
	}
	t.Err++
	if t.Codes == nil {
		t.Codes = map[string]int64{}
	}
	status, _ := num(r[cErrStatus])
	t.Codes[errCode(errName, status)]++
}

// capPending drops the oldest pending messages beyond pendingCap, counting each as a source error.
func (s *DBSource) capPending(errs map[string]int) {
	p := s.st.Pending
	if len(p) <= pendingCap {
		return
	}
	ids := make([]string, 0, len(p))
	for id := range p {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if p[ids[i]].Row != p[ids[j]].Row {
			return p[ids[i]].Row < p[ids[j]].Row
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids[:len(ids)-pendingCap] {
		delete(p, id)
		errs[core.ReasonOther]++
	}
}

// errCode maps opencode's error name (and an APIError's HTTP status) to the contract's bounded `code`. The
// error's message and body are never read.
func errCode(name string, status float64) string {
	switch name {
	case "ProviderAuthError":
		return "auth"
	case "APIError":
		switch status {
		case 429:
			return "rate_limited"
		case 401, 403:
			return "auth"
		case 503, 529:
			return "overloaded"
		}
	}
	return "other"
}

var (
	providerOK = regexp.MustCompile(`^[a-z0-9._-]{1,48}$`)
	modelOK    = regexp.MustCompile(`^[a-z0-9._:/@\[\]-]{1,64}$`)
)

// providerLabel bounds the stored provider id: lowercase, safe charset, ≤ 48 bytes; missing is "unknown" (as for
// the router and the contract's model rule), anything else unsafe is "other".
func providerLabel(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	switch {
	case p == "":
		return "unknown"
	case !providerOK.MatchString(p):
		return "other"
	}
	return p
}

// modelLabel bounds the stored model id: lowercase, safe charset, ≤ 64 bytes, else "other"; missing = "unknown".
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

// labelFor returns the model label a message of (provider, model) counts into. A pair keeps its own series while
// it is among the MaxModels named pairs of the profile; a new pair takes a free slot, or the slot of a pair unused
// for a day (that pair's series ends; its total is not moved), else it counts into model="other" under its
// provider. Totals never move between series, so no counter goes backwards (contract cardinality rule 3).
func (s *DBSource) labelFor(provider, model string, now time.Time) string {
	if model == "other" {
		return model
	}
	named := s.st.Named
	key := provider + "\x00" + model
	if _, ok := named[key]; ok {
		named[key] = now.Unix()
		return model
	}
	limit := s.MaxModels
	if limit <= 0 {
		limit = 20
	}
	if len(named) >= limit {
		victim, oldest := "", now.Unix()
		for k, last := range named {
			if last < oldest || (last == oldest && k < victim) {
				victim, oldest = k, last
			}
		}
		if now.Sub(time.Unix(oldest, 0)) < staleModel {
			return "other"
		}
		delete(named, victim)
		delete(s.st.Totals, victim)
	}
	named[key] = now.Unix()
	return model
}

// samples emits every total. Cost appears only for a series that has reported a non-zero cost: subscription
// providers report 0, and a 0 there would read as "free" rather than "not reported".
func (s *DBSource) samples() []core.Sample {
	keys := make([]string, 0, len(s.st.Totals))
	for k := range s.st.Totals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []core.Sample
	for _, k := range keys {
		provider, model, _ := strings.Cut(k, "\x00")
		t := s.st.Totals[k]
		base := func(extra ...core.Label) []core.Label {
			return append([]core.Label{{Name: "host", Value: s.Host}, {Name: "harness", Value: Harness},
				{Name: "profile", Value: s.Alias}, {Name: "account", Value: s.Account},
				{Name: "provider", Value: provider}, {Name: "model", Value: model}}, extra...)
		}
		add := func(name string, v float64, extra ...core.Label) {
			out = append(out, core.Sample{Name: name, Labels: base(extra...), Value: v})
		}
		for _, tv := range []struct {
			typ string
			v   int64
		}{{"input", t.In}, {"output", t.Out}, {"reasoning", t.Reasoning}, {"cache_read", t.CacheRead}, {"cache_write", t.CacheW}} {
			add("agent_tokens_total", float64(tv.v), core.Label{Name: "type", Value: tv.typ})
		}
		add("agent_requests_total", float64(t.OK), core.Label{Name: "outcome", Value: "ok"})
		add("agent_requests_total", float64(t.Err), core.Label{Name: "outcome", Value: "error"})
		codes := make([]string, 0, len(t.Codes))
		for c := range t.Codes {
			codes = append(codes, c)
		}
		sort.Strings(codes)
		for _, c := range codes {
			add("agent_request_errors_total", float64(t.Codes[c]), core.Label{Name: "code", Value: c})
		}
		if t.Cost > 0 {
			add("agent_cost_usd_total", t.Cost)
		}
	}
	return out
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// num returns a finite JSON number.
func num(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		if !math.IsNaN(x) && !math.IsInf(x, 0) {
			return x, true
		}
	}
	return 0, false
}

// count returns a token count: a non-negative integer, else 0.
func count(v any) int64 {
	f, ok := num(v)
	if !ok || f <= 0 || f > 1<<53 {
		return 0
	}
	return int64(f)
}
