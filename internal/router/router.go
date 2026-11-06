// Package router reads a 9router database read-only (`router-9router`): requests, tokens and connection health.
//
// What is read (specs/sources.md, "9router"): usageHistory rows after a persisted id cursor, at most batch rows per
// cycle, columns id, provider, model, connectionId, promptTokens, completionTokens, cost and status; and every
// providerConnections row with id, provider, name, isActive, email (email mode only) and two scalars of the JSON
// column data: $.testStatus and $.rateLimitedUntil. usageHistory.apiKey, tokens, meta and providerConnections.data as
// a whole are not in the allow-list, so sqlro refuses them before any SQL is built.
package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/events"
	"github.com/moophat/agent-quota-exporter/internal/sqlro"
)

const (
	SourceName = "router-9router"
	Harness    = config.HarnessRouter

	batch      = 5000 // usageHistory rows per cycle; the rest is read next cycle
	maxConns   = 50   // connections with their own account label per profile; the rest fold into "other"
	connRows   = 1000 // providerConnections rows read per cycle
	staleModel = 24 * time.Hour
)

// allow is everything this plugin may read. Never add usageHistory.apiKey, tokens or meta, or
// providerConnections.data as a bare Col: they hold keys, tokens or request content.
var allow = sqlro.Allow{
	"usageHistory": {sqlro.Col("id"), sqlro.Col("provider"), sqlro.Col("model"), sqlro.Col("connectionId"),
		sqlro.Col("promptTokens"), sqlro.Col("completionTokens"), sqlro.Col("cost"), sqlro.Col("status")},
	"providerConnections": {sqlro.Col("id"), sqlro.Col("provider"), sqlro.Col("name"), sqlro.Col("email"),
		sqlro.Col("isActive"), sqlro.JSON{Col: "data", Path: "$.testStatus"},
		sqlro.JSON{Col: "data", Path: "$.rateLimitedUntil"}},
}

var usageCols = []sqlro.Expr{sqlro.Col("id"), sqlro.Col("provider"), sqlro.Col("model"), sqlro.Col("connectionId"),
	sqlro.Col("promptTokens"), sqlro.Col("completionTokens"), sqlro.Col("cost"), sqlro.Col("status")}

// Build returns the sources for one resolved 9router profile. r.Dir is the .9router directory.
func Build(cfg *config.Config, r config.Resolved, host string) []core.Source {
	return []core.Source{&Source{
		Host:      host,
		Alias:     r.Alias,
		DB:        filepath.Join(r.Dir, "db", "data.sqlite"),
		EmailMode: cfg.Metrics.AccountLabel == "email",
		MaxModels: cfg.Metrics.MaxModelsPerProfile,
		Interval:  cfg.SourceInterval(SourceName, cfg.Defaults.UsageInterval.D()),
	}}
}

// Source is `router-9router` on one profile.
//
// Counting rules: the first successful open sets the cursor to the newest usageHistory id, so history is never
// replayed; later cycles count rows after the cursor in id order. A row's account and model labels are decided
// when it is counted and never change, so no counter moves between series.
type Source struct {
	Host       string
	Alias      string
	DB         string // <dir>/db/data.sqlite
	EmailMode  bool   // metrics.account_label: email
	MaxModels  int
	Interval   time.Duration
	Clock      func() time.Time // optional completion clock
	EventStore *events.Store    // nil when events are disabled

	mu         sync.Mutex
	st         *state     // loaded from the persisted blob on the first collect
	facts      []connFact // cycle-local raw connection facts, never persisted with metrics
	connCapped bool
}

// health is what the last successful read of providerConnections said about one labelled connection.
type health struct {
	Label    string `json:"l"`
	Provider string `json:"p"`
	Enabled  *bool  `json:"enabled,omitempty"` // nil in state written before this signal existed
	OK       bool   `json:"ok"`                // active and last test not failed
	Until    int64  `json:"u,omitempty"`       // rateLimitedUntil, Unix seconds; 0 = none
}

type total struct {
	In   int64   `json:"in"`
	Out  int64   `json:"out"`
	OK   int64   `json:"ok"`
	Err  int64   `json:"err"`
	Cost float64 `json:"cost,omitempty"`
}

type state struct {
	Generation    int64             `json:"generation"`
	DatabaseID    string            `json:"database_id,omitempty"`
	Pending       []json.RawMessage `json:"pending,omitempty"`
	Capture       int64             `json:"capture"`
	CaptureInit   bool              `json:"capture_init"`
	PendingCursor int64             `json:"pending_cursor"`
	Init          bool              `json:"init"`
	Cursor        int64             `json:"cursor"` // last counted usageHistory id
	Seq           int64             `json:"seq"`
	Run           int64             `json:"run"`
	// First is when each connection (by hashed id) was first seen, as a sequence: the oldest maxConns get labels.
	First  map[string]int64  `json:"first"`
	Health []health          `json:"health"`
	Totals map[string]*total `json:"totals"` // account \x00 provider \x00 model
	Models map[string]int64  `json:"models"` // named model -> last used, Unix seconds
}

func (s *Source) Name() string               { return SourceName }
func (s *Source) Harness() string            { return Harness }
func (s *Source) Profile() string            { return s.Alias }
func (s *Source) MinInterval() time.Duration { return s.Interval }
func (s *Source) Network() bool              { return false }

// SetEventStore enables private durable event capture for this source. A nil store
// preserves the metrics-only behavior used when event recording is disabled.
func (s *Source) SetEventStore(store *events.Store) { s.EventStore = store }

func (s *Source) load(raw json.RawMessage) *state {
	st := &state{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, st) // unreadable plugin state = start over, without replay
	}
	if st.First == nil {
		st.First = map[string]int64{}
	}
	if st.Totals == nil {
		st.Totals = map[string]*total{}
	}
	if st.Models == nil {
		st.Models = map[string]int64{}
	}
	return st
}

// Collect opens the database for this cycle only, reads connection health and the next batch of usage rows.
func (s *Source) Collect(ctx context.Context, now time.Time, raw json.RawMessage) (core.Result, json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == nil {
		s.st = s.load(raw)
	}
	st := s.st
	if s.EventStore != nil && st.Init && !st.CaptureInit {
		st.CaptureInit, st.Capture = true, st.Cursor
	}
	errs := map[string]int{}

	// Generate a stable run id for this cycle's collect event.
	st.Run++
	cycleID := fmt.Sprintf("%s\x00%s\x00%s\x00%d", SourceName, s.Host, s.Alias, st.Run)
	h := sha256.Sum256([]byte(cycleID))
	runID := hex.EncodeToString(h[:16])
	started := now
	wallStart := time.Now()

	db, err := sqlro.Open(s.DB, allow)
	if errors.Is(err, sqlro.ErrIdle) {
		// An odd state (one sidecar missing): serve what is persisted this cycle rather than open it.
		return s.result(now, errs), nil, nil
	}
	if err != nil {
		return core.Result{}, nil, err
	}
	defer db.Close()
	if info, statErr := os.Stat(s.DB); statErr == nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			identity := fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
			if st.DatabaseID != "" && st.DatabaseID != identity {
				st.Generation++
				st.Cursor, st.Capture = 0, 0
			}
			st.DatabaseID = identity
		}
	}
	if ctx.Err() != nil {
		return core.Result{}, nil, core.Fail(core.ReasonTimeout)
	}

	conns, err := s.readConns(db, now, errs)
	if err != nil {
		return core.Result{}, nil, err
	}
	top, err := db.MaxInt("usageHistory", "id")
	if err != nil {
		return core.Result{}, nil, err
	}

	// usageEnvs accumulates usage events for this cycle; only populated when EventStore is set.
	var usageEnvs []json.RawMessage
	var rowsAppended int64
	var backlogRows int64
	var skipped int64
	startup := false
	// Retry the exact retained envelopes, independently of this cycle's metrics.
	var captureErr error
	if s.EventStore != nil && len(st.Pending) > 0 {
		captureErr = s.appendEvents(s.EventStore, st.Pending)
		if captureErr == nil {
			st.Pending = nil
			st.Capture = st.PendingCursor
		}
	}

	switch {
	case !st.Init:
		st.Cursor, st.Init = top, true // first start: count from now on
		startup = top > 0
	case top < st.Cursor:
		st.Cursor = top // the table was replaced or its newest rows deleted: never re-count, never wait
		st.Generation++
	case top > st.Cursor:
		rows, err := db.After("usageHistory", "id", st.Cursor, usageCols, batch+1)
		if err != nil {
			return core.Result{}, nil, err
		}
		if len(rows) > batch {
			backlogRows = 1
			rows = rows[:batch]
		}
		for _, r := range rows {
			rowID, env := s.count(r, conns, now, errs)
			if s.EventStore != nil && env != nil {
				usageEnvs = append(usageEnvs, env)
			}
			if rowID > 0 {
				rowsAppended++
			} else {
				skipped++
			}
		}
	}
	s.prune(conns)
	if s.EventStore != nil && len(st.Pending) == 0 {
		usageEnvs = nil
		rowsAppended, skipped, backlogRows = 0, 0, 0
		if !st.CaptureInit {
			st.CaptureInit = true
			st.Capture = top
			startup = top > 0
		}
		if top < st.Capture {
			st.Capture = top
		}
		rows, readErr := db.After("usageHistory", "id", st.Capture, usageCols, batch+1)
		if readErr != nil {
			return s.result(now, errs), nil, readErr
		}
		if len(rows) > batch {
			backlogRows = 1
			rows = rows[:batch]
		}
		// Reuse parsing against a private snapshot: capture catch-up must not
		// increment already published metric totals a second time.
		snapshot, _ := json.Marshal(st)
		s.st = s.load(snapshot)
		captureErrors := map[string]int{}
		for _, r := range rows {
			id, env := s.count(r, conns, now, captureErrors)
			if id > 0 {
				usageEnvs = append(usageEnvs, env)
				rowsAppended++
			} else {
				skipped++
			}
		}
		st.PendingCursor = st.Capture
		if len(rows) > 0 {
			st.PendingCursor, _ = toInt(rows[len(rows)-1][0])
		}
		s.st = st
	}

	// Emit events after the cursor is advanced, before returning.
	if s.EventStore != nil {
		ended := now.Add(time.Since(wallStart))
		if s.Clock != nil {
			ended = s.Clock()
		}
		connFacts := s.facts
		collectEnv := s.collectEvent(runID, started, ended, rowsAppended, backlogRows, errs, connFacts)
		collectEnv = coverageEvent(collectEnv, startup, skipped)
		if s.connCapped {
			var e map[string]any
			_ = json.Unmarshal(collectEnv, &e)
			d := e["data"].(map[string]any)
			reasons, _ := d["omission_reasons"].([]any)
			d["omission_reasons"] = append(reasons, "connection_scan_cap")
			d["completion"] = "partial"
			collectEnv, _ = json.Marshal(e)
		}
		for i := range usageEnvs {
			usageEnvs[i] = collectedEvent(usageEnvs[i], ended)
		}
		all := append(usageEnvs, collectEnv)
		// Keep a failed bounded batch in state. While it is blocked, leave later
		// source rows queued for capture, but continue their metric accounting.
		if len(st.Pending) == 0 {
			st.Pending = all
			captureErr = s.appendEvents(s.EventStore, st.Pending)
			if captureErr == nil {
				st.Pending = nil
				st.Capture = st.PendingCursor
			}
		}
	}
	blob, err := json.Marshal(st)
	if err != nil {
		return core.Result{}, nil, core.Fail(core.ReasonOther)
	}
	res := s.result(now, errs)
	if captureErr != nil {
		res.ObservedAt = now
		return res, blob, core.Fail(core.ReasonOther)
	}
	return res, blob, nil
}

// count adds one usageHistory row. The cursor advances past every row, including one it cannot parse.
// It returns the row's id (0 on parse failure) and the event envelope (nil when the row is unparseable).
func (s *Source) count(r []any, conns map[string]string, now time.Time, errs map[string]int) (int64, json.RawMessage) {
	st := s.st
	id, ok := toInt(r[0])
	if !ok {
		errs[core.ReasonParse]++
		return 0, nil
	}
	st.Cursor = max(st.Cursor, id)
	in, ok1 := toInt(r[4])
	out, ok2 := toInt(r[5])
	cost, ok3 := toFloat(r[6])
	if !ok1 || !ok2 || !ok3 {
		errs[core.ReasonParse]++
		return 0, nil
	}
	account := "unknown"
	if cid := toStr(r[3]); cid != "" {
		if account = conns[cid]; account == "" {
			account = "other" // a connection that no longer exists, or one past the connections read
		}
	}
	provider := providerLabel(toStr(r[1]))
	model := s.modelFor(modelLabel(toStr(r[2])), now)
	key := account + "\x00" + provider + "\x00" + model
	t := st.Totals[key]
	if t == nil {
		t = &total{}
		st.Totals[key] = t
	}
	t.In += max(in, 0)
	t.Out += max(out, 0)
	t.Cost += max(cost, 0)
	rowOK := statusOK(r[7])
	if rowOK {
		t.OK++
	} else {
		t.Err++
	}

	// Build event envelope if the event store is wired.
	var env json.RawMessage
	if s.EventStore != nil {
		env = s.usageEvent(id, toStr(r[1]), toStr(r[2]), toStr(r[3]), toStr(r[7]), provider, model, account, max(in, 0), max(out, 0), rowOK, max(cost, 0), now)
		var e map[string]any
		_ = json.Unmarshal(env, &e)
		revisionBytes, _ := json.Marshal(r)
		revision := sha256.Sum256(revisionBytes)
		loc := e["locator"].(map[string]any)
		loc["generation"] = st.Generation
		loc["revision"] = hex.EncodeToString(revision[:])
		identity, _ := json.Marshal([]any{SourceName, s.Host, s.Alias, Harness, loc})
		digest := sha256.Sum256(identity)
		e["event_id"] = hex.EncodeToString(digest[:])
		if r[6] == nil {
			e["data"].(map[string]any)["reported_cost"] = nil
		}
		env, _ = json.Marshal(e)
	}
	return id, env
}

// modelFor returns the series a row of this model counts into: its own while it is among the profile's MaxModels
// named models (across connections), else "other". A model unused for a day gives its slot to a new one; its
// series end there (their totals are dropped, never moved), as for claude-transcripts.
func (s *Source) modelFor(model string, now time.Time) string {
	if model == "other" {
		return model
	}
	named := s.st.Models
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
		for k := range s.st.Totals {
			if strings.HasSuffix(k, "\x00"+victim) {
				delete(s.st.Totals, k)
			}
		}
	}
	named[model] = now.Unix()
	return model
}

// prune ends the counter series of accounts that no longer have a label (connection deleted, renamed or pushed
// out of the cap). Counters of a label that comes back restart from zero, which Prometheus reads as a reset.
func (s *Source) prune(conns map[string]string) {
	live := map[string]bool{"other": true, "unknown": true}
	for _, l := range conns {
		live[l] = true
	}
	for k := range s.st.Totals {
		if a, _, _ := strings.Cut(k, "\x00"); !live[a] {
			delete(s.st.Totals, k)
		}
	}
}

func (s *Source) result(now time.Time, errs map[string]int) core.Result {
	res := core.Result{Samples: s.samples(now)}
	if len(errs) > 0 {
		res.Errors = errs
	}
	return res
}

func (s *Source) labels(account, provider string, extra ...core.Label) []core.Label {
	ls := []core.Label{{Name: "host", Value: s.Host}, {Name: "harness", Value: Harness}, {Name: "profile", Value: s.Alias},
		{Name: "account", Value: account}, {Name: "provider", Value: provider}}
	return append(ls, extra...)
}

func (s *Source) samples(now time.Time) []core.Sample {
	var out []core.Sample
	for _, h := range s.st.Health {
		if h.Enabled != nil {
			enabled := 0.0
			if *h.Enabled {
				enabled = 1
			}
			out = append(out, core.Sample{Name: "agent_router_connection_enabled", Labels: s.labels(h.Label, h.Provider), Value: enabled})
		}
		up, limited := 0.0, h.Until > now.Unix()
		if h.OK && !limited {
			up = 1
		}
		out = append(out, core.Sample{Name: "agent_router_connection_up", Labels: s.labels(h.Label, h.Provider), Value: up})
		if limited {
			out = append(out, core.Sample{Name: "agent_router_rate_limited_until_timestamp_seconds",
				Labels: s.labels(h.Label, h.Provider), Value: float64(h.Until)})
		}
	}
	keys := make([]string, 0, len(s.st.Totals))
	for k := range s.st.Totals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts := strings.SplitN(k, "\x00", 3)
		t := s.st.Totals[k]
		base := func(extra ...core.Label) []core.Label {
			return s.labels(parts[0], parts[1], append([]core.Label{{Name: "model", Value: parts[2]}}, extra...)...)
		}
		out = append(out,
			core.Sample{Name: "agent_tokens_total", Labels: base(core.Label{Name: "type", Value: "input"}), Value: float64(t.In)},
			core.Sample{Name: "agent_tokens_total", Labels: base(core.Label{Name: "type", Value: "output"}), Value: float64(t.Out)},
			core.Sample{Name: "agent_requests_total", Labels: base(core.Label{Name: "outcome", Value: "ok"}), Value: float64(t.OK)},
			core.Sample{Name: "agent_requests_total", Labels: base(core.Label{Name: "outcome", Value: "error"}), Value: float64(t.Err)},
		)
		if t.Cost > 0 { // a router without a price for the model records 0: no cost series until one is seen
			out = append(out, core.Sample{Name: "agent_cost_usd_total", Labels: base(), Value: t.Cost})
		}
	}
	return out
}

// hashID is the account rule shared with claude-code: "acct-" + the first 8 hex digits of sha256(id).
func hashID(id string) string {
	h := sha256.Sum256([]byte(id))
	return "acct-" + hex.EncodeToString(h[:])[:8]
}

// stateKey keys a connection in the persisted state without storing its id.
func stateKey(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:8])
}
