package zai

import (
	"sort"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

const (
	// Harness and Provider are the contract label values of every series of this package.
	Harness  = "zai"
	Provider = "zai"
)

// ident is the profile identity every series carries.
type ident struct{ Host, Alias, Account string }

func (i ident) labels(extra ...core.Label) []core.Label {
	ls := []core.Label{{Name: "host", Value: i.Host}, {Name: "harness", Value: Harness}, {Name: "profile", Value: i.Alias},
		{Name: "account", Value: i.Account}, {Name: "provider", Value: Provider}}
	return append(ls, extra...)
}

// state is the persisted blob: the last good reading and when it was taken.
type state struct {
	Reading *reading `json:"reading,omitempty"`
	LastAt  int64    `json:"last_at,omitempty"` // Unix ms of the last good read
}

// serve builds the samples of the persisted reading as of now. A reading older than limit is not served; a window
// whose reset has passed is dropped, since its utilization no longer describes the new window.
func serve(id ident, st *state, limit time.Duration, now time.Time) []core.Sample {
	if st == nil || st.Reading == nil || st.LastAt == 0 {
		return nil
	}
	at := time.UnixMilli(st.LastAt)
	if now.Sub(at) > limit {
		return nil
	}
	ws := append([]window(nil), st.Reading.Windows...)
	sort.Slice(ws, func(i, j int) bool {
		if ws[i].Window != ws[j].Window {
			return ws[i].Window < ws[j].Window
		}
		return ws[i].Scope < ws[j].Scope
	})
	var out []core.Sample
	for _, w := range ws {
		if w.Reset > 0 && w.Reset <= now.Unix() {
			continue
		}
		ls := id.labels(core.Label{Name: "window", Value: w.Window}, core.Label{Name: "scope", Value: w.Scope})
		add := func(name string, v *float64) {
			if v != nil {
				out = append(out, core.Sample{Name: name, Labels: ls, Value: *v})
			}
		}
		add("agent_quota_utilization_ratio", w.Ratio)
		if w.Reset > 0 {
			r := float64(w.Reset)
			add("agent_quota_reset_timestamp_seconds", &r)
		}
		add("agent_quota_used_calls", w.Used)
		add("agent_quota_limit_calls", w.Limit)
	}
	out = append(out,
		core.Sample{Name: "agent_quota_observed_timestamp_seconds", Labels: id.labels(), Value: float64(at.Unix())},
		core.Sample{Name: "agent_account_info", Labels: id.labels(core.Label{Name: "plan", Value: st.Reading.Level}), Value: 1})
	return out
}
