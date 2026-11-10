package zai

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// The window mapping is deliberately closed. The unit codes are inferred from live responses (unit 3 number 5 is the
// 5-hour window; unit 6 number 1 resets a week out; unit 5 number 1 is the monthly tool-call allowance), not
// documented by z.ai. Anything else is not exported and is counted as a parse error.
const (
	typeTokens = "TOKENS_LIMIT"
	typeTime   = "TIME_LIMIT"

	unitHour  = 3
	unitMonth = 5
	unitWeek  = 6

	maxNumber = 1000 // far above any real window; keeps the label short
)

// Bounds of a plausible nextResetTime, Unix seconds: 2000-01-01 and 2100-01-01.
const earliestReset, latestReset = 946684800, 4102444800

// window is one exported quota window. Pointers are nil when the vendor did not report the value: absent, never 0.
type window struct {
	Window string   `json:"w"`
	Scope  string   `json:"s"`
	Ratio  *float64 `json:"r,omitempty"`
	Reset  int64    `json:"x,omitempty"` // Unix seconds; 0 = none reported
	Used   *float64 `json:"u,omitempty"` // calls, TIME_LIMIT only
	Limit  *float64 `json:"l,omitempty"`
}

// reading is one successful read, in the persisted form.
type reading struct {
	Level   string   `json:"level"`
	Windows []window `json:"windows"`
}

type parsed struct {
	Reading *reading
	Unknown int // limits left out of the mapping
}

type envelope struct {
	Code    json.RawMessage `json:"code"`
	Success *bool           `json:"success"`
	Data    *struct {
		Level  json.RawMessage   `json:"level"`
		Limits []json.RawMessage `json:"limits"`
	} `json:"data"`
}

type rawLimit struct {
	Type          string          `json:"type"`
	Unit          json.RawMessage `json:"unit"`
	Number        json.RawMessage `json:"number"`
	Usage         json.RawMessage `json:"usage"`
	CurrentValue  json.RawMessage `json:"currentValue"`
	Percentage    json.RawMessage `json:"percentage"`
	NextResetTime json.RawMessage `json:"nextResetTime"`
}

// parseQuota decodes one response body. reason is "" on success, else the contract reason for the failure:
// an HTTP 200 body is only data when it is a success:true envelope with limits.
func parseQuota(body []byte) (parsed, string) {
	var env envelope
	if json.Unmarshal(body, &env) != nil || env.Success == nil {
		return parsed{}, core.ReasonParse
	}
	if !*env.Success {
		return parsed{}, classifyCode(env.Code)
	}
	if env.Data == nil || len(env.Data.Limits) == 0 {
		return parsed{}, core.ReasonParse
	}
	p := parsed{Reading: &reading{Level: planOf(env.Data.Level)}}
	merged := map[string]*window{}
	var order []string
	for _, raw := range env.Data.Limits {
		w, ok := mapLimit(raw)
		if !ok {
			p.Unknown++
			continue
		}
		k := w.Window + "\x00" + w.Scope
		m, dup := merged[k]
		if !dup {
			merged[k] = &w
			order = append(order, k)
			continue
		}
		m.Ratio = maxPtr(m.Ratio, w.Ratio)
		m.Used = maxPtr(m.Used, w.Used)
		m.Limit = maxPtr(m.Limit, w.Limit)
		if w.Reset != 0 && (m.Reset == 0 || w.Reset < m.Reset) {
			m.Reset = w.Reset
		}
	}
	for _, k := range order {
		p.Reading.Windows = append(p.Reading.Windows, *merged[k])
	}
	return p, ""
}

// classifyCode maps the code of a success:false envelope. 1000-1004 are the authentication failures seen live
// (1000 bad key, 1001 no header); 1302 and 1305 are the rate-limit and overload codes of z.ai's own docs.
func classifyCode(raw json.RawMessage) string {
	n, ok := number(raw)
	switch {
	case !ok:
		return core.ReasonUpstream
	case n >= 1000 && n <= 1004:
		return core.ReasonAuthExpired
	case n == 1302 || n == 1305:
		return core.ReasonRateLimited
	}
	return core.ReasonUpstream
}

func planOf(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "unknown"
	}
	switch s = strings.ToLower(s); s {
	case "lite", "pro", "max", "team":
		return s
	}
	return "unknown"
}

func mapLimit(raw json.RawMessage) (window, bool) {
	var l rawLimit
	if json.Unmarshal(raw, &l) != nil {
		return window{}, false
	}
	unit, uok := number(l.Unit)
	num, nok := number(l.Number)
	if !uok || !nok || num < 1 || num > maxNumber || num != math.Trunc(num) {
		return window{}, false
	}
	n := int(num)
	var w window
	switch {
	case l.Type == typeTokens && unit == unitHour:
		w = window{Window: strconv.Itoa(n) + "h", Scope: "all"}
	case l.Type == typeTokens && unit == unitWeek:
		w = window{Window: strconv.Itoa(7*n) + "d", Scope: "all"}
	case l.Type == typeTime && unit == unitMonth && n == 1:
		w = window{Window: "monthly", Scope: "tool_calls"}
	default:
		return window{}, false
	}
	if p, ok := number(l.Percentage); ok && p >= 0 {
		r := p / 100
		w.Ratio = &r
	}
	if l.Type == typeTime {
		cur, cok := number(l.CurrentValue)
		cap, kok := number(l.Usage)
		if cok && cur >= 0 {
			w.Used = &cur
		}
		if kok && cap >= 0 {
			w.Limit = &cap
		}
		if w.Ratio == nil && cok && kok && cur >= 0 && cap > 0 {
			r := cur / cap
			w.Ratio = &r
		}
	}
	if ms, ok := number(l.NextResetTime); ok {
		if sec := math.Floor(ms / 1000); sec >= earliestReset && sec <= latestReset {
			w.Reset = int64(sec)
		}
	}
	return w, true
}

// number reads a JSON number or a numeric string; NaN, Inf and anything else are not numbers.
func number(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var v float64
	if json.Unmarshal(raw, &v) != nil {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return 0, false
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return 0, false
		}
		v = f
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

func maxPtr(a, b *float64) *float64 {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *b > *a:
		return b
	}
	return a
}
