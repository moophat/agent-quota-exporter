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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
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
	Host      string
	Alias     string
	DB        string // <dir>/db/data.sqlite
	EmailMode bool   // metrics.account_label: email
	MaxModels int
	Interval  time.Duration

	mu sync.Mutex
	st *state // loaded from the persisted blob on the first collect
}

// health is what the last successful read of providerConnections said about one labelled connection.
type health struct {
	Label    string `json:"l"`
	Provider string `json:"p"`
	OK       bool   `json:"ok"`          // active and last test not failed
	Until    int64  `json:"u,omitempty"` // rateLimitedUntil, Unix seconds; 0 = none
}

type total struct {
	In   int64   `json:"in"`
	Out  int64   `json:"out"`
	OK   int64   `json:"ok"`
	Err  int64   `json:"err"`
	Cost float64 `json:"cost,omitempty"`
}

type state struct {
	Init   bool  `json:"init"`
	Cursor int64 `json:"cursor"` // last counted usageHistory id
	Seq    int64 `json:"seq"`
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
	errs := map[string]int{}

	db, err := sqlro.Open(s.DB, allow)
	if errors.Is(err, sqlro.ErrIdle) {
		// An odd state (one sidecar missing): serve what is persisted this cycle rather than open it.
		return s.result(now, errs), nil, nil
	}
	if err != nil {
		return core.Result{}, nil, err
	}
	defer db.Close()
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
	switch {
	case !st.Init:
		st.Cursor, st.Init = top, true // first start: count from now on
	case top < st.Cursor:
		st.Cursor = top // the table was replaced or its newest rows deleted: never re-count, never wait
	case top > st.Cursor:
		rows, err := db.After("usageHistory", "id", st.Cursor, usageCols, batch)
		if err != nil {
			return core.Result{}, nil, err
		}
		for _, r := range rows {
			s.count(r, conns, now, errs)
		}
	}
	s.prune(conns)

	blob, err := json.Marshal(st)
	if err != nil {
		return core.Result{}, nil, core.Fail(core.ReasonOther)
	}
	return s.result(now, errs), blob, nil
}

// count adds one usageHistory row. The cursor advances past every row, including one it cannot parse.
func (s *Source) count(r []any, conns map[string]string, now time.Time, errs map[string]int) {
	st := s.st
	id, ok := toInt(r[0])
	if !ok {
		errs[core.ReasonParse]++
		return
	}
	st.Cursor = max(st.Cursor, id)
	in, ok1 := toInt(r[4])
	out, ok2 := toInt(r[5])
	cost, ok3 := toFloat(r[6])
	if !ok1 || !ok2 || !ok3 {
		errs[core.ReasonParse]++
		return
	}
	account := "unknown"
	if cid := toStr(r[3]); cid != "" {
		if account = conns[cid]; account == "" {
			account = "other" // a connection that no longer exists, or one past the connections read
		}
	}
	key := account + "\x00" + providerLabel(toStr(r[1])) + "\x00" + s.modelFor(modelLabel(toStr(r[2])), now)
	t := st.Totals[key]
	if t == nil {
		t = &total{}
		st.Totals[key] = t
	}
	t.In += max(in, 0)
	t.Out += max(out, 0)
	t.Cost += max(cost, 0)
	if statusOK(r[7]) {
		t.OK++
	} else {
		t.Err++
	}
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
