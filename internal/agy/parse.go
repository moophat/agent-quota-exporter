package agy

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// row is one quota window after mapping and merging. It is also the persisted form.
type row struct {
	Window string  `json:"w"`
	Scope  string  `json:"s"`
	Ratio  float64 `json:"r"`
	Reset  int64   `json:"t,omitempty"`
}

var windowSafe = regexp.MustCompile(`^[a-z0-9]{1,16}$`)

// scopes is the fixed vocabulary of bucket groups, taken from the bucket id prefix (`gemini-weekly` → gemini).
var scopes = map[string]bool{"gemini": true, "3p": true}

// output holds only the fields the plugin reads. Content fields (`response`, every `name` and `description`)
// are not declared, so the decoder drops them.
type output struct {
	NumTurns json.RawMessage `json:"num_turns"`
	Usage    json.RawMessage `json:"usage"`
	Command  json.RawMessage `json:"command"`
}

type command struct {
	Name json.RawMessage `json:"name"`
	Data json.RawMessage `json:"data"`
}

type data struct {
	Groups []struct {
		Buckets []struct {
			ID                string   `json:"id"`
			Window            string   `json:"window"`
			RemainingFraction *float64 `json:"remaining_fraction"`
			ResetTime         string   `json:"reset_time"`
		} `json:"buckets"`
	} `json:"groups"`
}

// parse maps the client's JSON to rows. It returns the number of buckets whose id is outside the scope
// vocabulary (served as scope "other").
//
// Before anything else it checks that the output is the zero-cost /usage answer: `command.name` is "usage" and
// both `num_turns` and `usage.total_tokens` are present and 0. Anything else means the client changed in a way
// that may spend model turns, so the source stops for GuardBackoff and nothing from that output is used.
func parse(b []byte) ([]row, int, error) {
	var o output
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, 0, core.Fail(core.ReasonParse)
	}
	var c command
	if !isZero(o.NumTurns) || !zeroTokens(o.Usage) || json.Unmarshal(o.Command, &c) != nil || !isString(c.Name, "usage") {
		return nil, 0, &core.SourceError{Reason: core.ReasonOther, Backoff: GuardBackoff}
	}
	var d data
	if err := json.Unmarshal(c.Data, &d); err != nil {
		return nil, 0, core.Fail(core.ReasonParse)
	}
	merged := map[[2]string]row{}
	unknown := 0
	for _, g := range d.Groups {
		for _, bk := range g.Buckets {
			if bk.RemainingFraction == nil {
				continue // no value: absent, never zero
			}
			window := strings.ToLower(bk.Window)
			if !windowSafe.MatchString(window) {
				window = "other"
			}
			scope, _, _ := strings.Cut(strings.ToLower(bk.ID), "-")
			if !scopes[scope] {
				scope = "other"
				unknown++
			}
			r := row{Window: window, Scope: scope, Ratio: max(0, 1-*bk.RemainingFraction)}
			if t, err := time.Parse(time.RFC3339Nano, bk.ResetTime); err == nil && t.Unix() > 0 {
				r.Reset = t.Unix()
			}
			// Mapping to "other" can make two buckets share a label set: keep the most used and the soonest reset.
			k := [2]string{window, scope}
			if prev, ok := merged[k]; ok {
				r.Ratio = max(r.Ratio, prev.Ratio)
				if r.Reset == 0 || (prev.Reset > 0 && prev.Reset < r.Reset) {
					r.Reset = prev.Reset
				}
			}
			merged[k] = r
		}
	}
	if len(merged) == 0 {
		return nil, unknown, core.Fail(core.ReasonParse)
	}
	rows := make([]row, 0, len(merged))
	for _, r := range merged {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Scope != rows[j].Scope {
			return rows[i].Scope < rows[j].Scope
		}
		return rows[i].Window < rows[j].Window
	})
	return rows, unknown, nil
}

// isZero reports whether raw is the JSON number 0 (a missing field is not zero).
func isZero(raw json.RawMessage) bool {
	var n *float64
	return len(raw) > 0 && json.Unmarshal(raw, &n) == nil && n != nil && *n == 0
}

func zeroTokens(raw json.RawMessage) bool {
	var u struct {
		TotalTokens json.RawMessage `json:"total_tokens"`
	}
	return len(raw) > 0 && json.Unmarshal(raw, &u) == nil && isZero(u.TotalTokens)
}

func isString(raw json.RawMessage, want string) bool {
	var s *string
	return len(raw) > 0 && json.Unmarshal(raw, &s) == nil && s != nil && *s == want
}
