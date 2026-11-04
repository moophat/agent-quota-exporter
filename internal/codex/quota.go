package codex

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// creditsFallback bounds credits from a reading with no window reset time (design section 8, verdict 1).
const creditsFallback = 7 * 24 * time.Hour

// rateLimits is Codex's RateLimitSnapshot (codex-rs/protocol), the structural fields only. limit_name and every
// other field are never decoded.
type rateLimits struct {
	LimitID   *string   `json:"limit_id"`
	Primary   *rlWindow `json:"primary"`
	Secondary *rlWindow `json:"secondary"`
	Credits   *struct {
		HasCredits bool            `json:"has_credits"`
		Unlimited  bool            `json:"unlimited"`
		Balance    json.RawMessage `json:"balance"` // a string in Codex today; a number is accepted too
	} `json:"credits"`
}

// rlWindow numbers are decoded as float64 so a vendor writing 300.0 does not fail the line.
type rlWindow struct {
	UsedPercent   *float64 `json:"used_percent"`
	WindowMinutes *float64 `json:"window_minutes"`
	ResetsAt      *float64 `json:"resets_at"`
}

// reading is the persisted quota of one scope: labels already mapped, so nothing unbounded is stored.
type reading struct {
	AtMs    int64     `json:"at"` // the token_count event's own time, Unix ms
	Scope   string    `json:"scope"`
	Windows []qWindow `json:"windows,omitempty"`
	Credits *credits  `json:"credits,omitempty"`
}

type qWindow struct {
	Window string  `json:"w"`
	Used   float64 `json:"u"`           // percent, as reported
	Reset  int64   `json:"r,omitempty"` // Unix seconds; 0 = none reported
}

type credits struct {
	Available bool     `json:"a"`
	Balance   *float64 `json:"b,omitempty"`
	Until     int64    `json:"until"` // Unix seconds: served up to here unless a newer reading replaces it
}

var scopeOK = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// scopeLabel maps limit_id: the main Codex limit (or none) covers everything; another id passes through only
// inside the fixed charset and length.
func scopeLabel(id *string) string {
	switch {
	case id == nil || *id == "" || *id == "codex":
		return "all"
	case scopeOK.MatchString(*id):
		return *id
	default:
		return "other"
	}
}

// windowLabel maps window_minutes to the contract's duration form: 300 → 5h, 10080 → 7d, else <n>d, <n>h or
// <n>m by the largest whole unit. Missing, non-positive or fractional minutes → "other".
func windowLabel(minutes *float64) string {
	if minutes == nil || *minutes <= 0 || *minutes != math.Trunc(*minutes) || *minutes > math.MaxInt32 {
		return "other"
	}
	m := int64(*minutes)
	switch {
	case m%1440 == 0:
		return strconv.FormatInt(m/1440, 10) + "d"
	case m%60 == 0:
		return strconv.FormatInt(m/60, 10) + "h"
	default:
		return strconv.FormatInt(m, 10) + "m"
	}
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// parseBalance accepts "12.5" or 12.5; anything else (null, text) is "not reported".
func parseBalance(raw json.RawMessage) (float64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	s := string(raw)
	if raw[0] == '"' {
		if json.Unmarshal(raw, &s) != nil {
			return 0, false
		}
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || !finite(v) {
		return 0, false
	}
	return v, true
}

func newReading(rl *rateLimits, at time.Time) *reading {
	r := &reading{AtMs: at.UnixMilli(), Scope: scopeLabel(rl.LimitID)}
	longest, longestReset := -1.0, int64(0)
	for _, w := range []*rlWindow{rl.Primary, rl.Secondary} {
		if w == nil || w.UsedPercent == nil || !finite(*w.UsedPercent) {
			continue
		}
		qw := qWindow{Window: windowLabel(w.WindowMinutes), Used: *w.UsedPercent}
		if w.ResetsAt != nil && finite(*w.ResetsAt) && *w.ResetsAt > 0 {
			qw.Reset = int64(*w.ResetsAt)
		}
		r.Windows = append(r.Windows, qw)
		m := 0.0
		if w.WindowMinutes != nil && finite(*w.WindowMinutes) {
			m = *w.WindowMinutes
		}
		if m > longest {
			longest, longestReset = m, qw.Reset
		}
	}
	if c := rl.Credits; c != nil {
		cr := &credits{Available: c.HasCredits || c.Unlimited, Until: longestReset}
		if cr.Until == 0 {
			cr.Until = at.Add(creditsFallback).Unix()
		}
		if !c.Unlimited {
			if b, ok := parseBalance(c.Balance); ok {
				cr.Balance = &b
			}
		}
		r.Credits = cr
	}
	return r
}

// maxScopes bounds the readings kept per profile; the scope whose newest reading is oldest goes first.
const maxScopes = 8

// observe takes a rate_limits reading at its event time into its scope. Per scope the newest event wins; an
// older one never replaces it, whatever order files are read in. Credits are kept from the scope's previous
// reading when the newer one reports none.
func (s *RolloutsSource) observe(rl *rateLimits, at time.Time) {
	r := newReading(rl, at)
	cur := s.st.Quotas[r.Scope]
	if cur != nil && r.AtMs < cur.AtMs {
		return
	}
	if r.Credits == nil && cur != nil {
		r.Credits = cur.Credits
	}
	s.st.Quotas[r.Scope] = r
	if len(s.st.Quotas) > maxScopes {
		victim := ""
		for sc, q := range s.st.Quotas {
			if v := s.st.Quotas[victim]; victim == "" || q.AtMs < v.AtMs || (q.AtMs == v.AtMs && sc < victim) {
				victim = sc
			}
		}
		delete(s.st.Quotas, victim)
	}
}

func minReset(a, b int64) int64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	default:
		return min(a, b)
	}
}

// quotaSamples serves the persisted readings at collect time (contract, "Passive sources are the one
// exception"): per scope, a window until its own reset passes, a window without a reset time for StaleAfter from
// its reading, credits until their Until. Windows that map to the same label set are merged (max utilization, min
// reset). The observed timestamp has no scope label, so it is the newest reading among the scopes still served.
func (s *RolloutsSource) quotaSamples(now time.Time) []core.Sample {
	scopes := make([]string, 0, len(s.st.Quotas))
	for sc := range s.st.Quotas {
		scopes = append(scopes, sc)
	}
	sort.Strings(scopes)
	var out []core.Sample
	var newest int64
	for _, sc := range scopes {
		q := s.st.Quotas[sc]
		if ss := s.scopeSamples(q, now); len(ss) > 0 {
			out = append(out, ss...)
			newest = max(newest, q.AtMs)
		}
	}
	if len(out) > 0 {
		out = append(out, core.Sample{Name: "agent_quota_observed_timestamp_seconds", Labels: s.labels(),
			Value: float64(time.UnixMilli(newest).Unix())})
	}
	return out
}

func (s *RolloutsSource) scopeSamples(q *reading, now time.Time) []core.Sample {
	at := time.UnixMilli(q.AtMs)
	merged := map[string]qWindow{}
	for _, w := range q.Windows {
		if w.Reset > 0 && w.Reset <= now.Unix() {
			continue
		}
		if w.Reset == 0 && now.Sub(at) > s.StaleAfter {
			continue
		}
		if m, ok := merged[w.Window]; ok {
			w.Used = max(w.Used, m.Used)
			w.Reset = minReset(w.Reset, m.Reset)
		}
		merged[w.Window] = w
	}
	windows := make([]string, 0, len(merged))
	for k := range merged {
		windows = append(windows, k)
	}
	sort.Strings(windows)
	var out []core.Sample
	for _, k := range windows {
		w := merged[k]
		ls := s.labels(core.Label{Name: "window", Value: w.Window}, core.Label{Name: "scope", Value: q.Scope})
		out = append(out, core.Sample{Name: "agent_quota_utilization_ratio", Labels: ls, Value: w.Used / 100})
		if w.Reset > 0 {
			out = append(out, core.Sample{Name: "agent_quota_reset_timestamp_seconds", Labels: ls, Value: float64(w.Reset)})
		}
	}
	if c := q.Credits; c != nil && now.Unix() < c.Until {
		ls := s.labels(core.Label{Name: "scope", Value: q.Scope})
		avail := 0.0
		if c.Available {
			avail = 1
		}
		out = append(out, core.Sample{Name: "agent_quota_credits_available", Labels: ls, Value: avail})
		if c.Balance != nil {
			out = append(out, core.Sample{Name: "agent_quota_credits_balance", Labels: ls, Value: *c.Balance})
		}
	}
	return out
}
