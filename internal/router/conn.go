package router

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/sqlro"
)

var (
	labelRe = regexp.MustCompile(`^[a-z0-9._-]{1,48}$`)
	modelRe = regexp.MustCompile(`^[a-z0-9._:/@\[\]-]{1,64}$`)
	emailRe = regexp.MustCompile(`^[a-z0-9._%+-]{1,64}@[a-z0-9.-]{1,63}$`)
)

// testStatus words as 9router writes them (data.testStatus). A failed word means the last test failed; a passing
// word is a pass, a pending test or none yet. Any other word reads as failed and counts as a parse error, so a
// new upstream state shows as a down connection instead of a silent up.
var (
	testPass = map[string]bool{"": true, "active": true, "ok": true, "success": true, "unknown": true, "testing": true}
	testFail = map[string]bool{"error": true, "expired": true, "unavailable": true}
)

// readConns reads every connection (up to connRows), refreshes the persisted health and returns connection id →
// account label for counting: the maxConns connections seen first have their own label, the rest are "other".
func (s *Source) readConns(db *sqlro.DB, now time.Time, errs map[string]int) (map[string]string, error) {
	cols := []sqlro.Expr{sqlro.Col("id"), sqlro.Col("provider"), sqlro.Col("name"), sqlro.Col("isActive"),
		sqlro.JSON{Col: "data", Path: "$.testStatus"}, sqlro.JSON{Col: "data", Path: "$.rateLimitedUntil"}}
	if s.EmailMode {
		cols = append(cols, sqlro.Col("email")) // selected only when an email may become the label
	}
	rows, err := db.After("providerConnections", "rowid", 0, cols, connRows)
	if err != nil {
		return nil, err
	}

	st := s.st
	type conn struct {
		id, key string
		row     []any
	}
	var cs []conn
	seen := map[string]bool{}
	for _, r := range rows {
		id := toStr(r[0])
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		k := stateKey(id)
		if _, ok := st.First[k]; !ok {
			st.Seq++
			st.First[k] = st.Seq
		}
		cs = append(cs, conn{id, k, r})
	}
	live := map[string]bool{}
	for _, c := range cs {
		live[c.key] = true
	}
	for k := range st.First {
		if !live[k] {
			delete(st.First, k) // deleted connection: its slot is free again
		}
	}
	sort.SliceStable(cs, func(i, j int) bool { return st.First[cs[i].key] < st.First[cs[j].key] })

	labels := make(map[string]string, len(cs))
	used := map[string]bool{"other": true, "unknown": true}
	var hs []health
	for i, c := range cs {
		l := ""
		if i < maxConns {
			email := ""
			if s.EmailMode {
				email = toStr(c.row[6])
			}
			l = s.connLabel(c.id, toStr(c.row[2]), email, used)
		}
		if l == "" {
			labels[c.id] = "other" // past the cap: counted, but no health series of its own
			continue
		}
		used[l] = true
		labels[c.id] = l
		hs = append(hs, connHealth(l, c.row, errs))
	}
	st.Health = hs
	return labels, nil
}

// keyLike reports a name that may be a credential pasted as a label: a common key prefix, or a run of 20 or more
// letters and digits. Such a connection is labelled by the hash of its id instead.
func keyLike(n string) bool {
	for _, p := range []string{"sk-", "sk_", "key-", "pk-", "rk-", "ghp_", "gho_", "xox", "aiza"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return longAlnum.MatchString(n)
}

var longAlnum = regexp.MustCompile(`[a-z0-9]{20,}`)

// connLabel picks the first free label of: the connection's name if label-safe, its email in email mode, the
// hash of its id. "" means all are taken.
func (s *Source) connLabel(id, name, email string, used map[string]bool) string {
	var cand []string
	if n := strings.ToLower(strings.TrimSpace(name)); labelRe.MatchString(n) && !keyLike(n) {
		cand = append(cand, n)
	}
	if e := strings.ToLower(strings.TrimSpace(email)); s.EmailMode && emailRe.MatchString(e) {
		cand = append(cand, e)
	}
	cand = append(cand, hashID(id))
	for _, c := range cand {
		if !used[c] {
			return c
		}
	}
	return ""
}

// connHealth: OK = isActive is 1 and the last test did not fail; Until = rateLimitedUntil if parseable.
func connHealth(label string, r []any, errs map[string]int) health {
	h := health{Label: label, Provider: providerLabel(toStr(r[1]))}
	active, _ := toInt(r[3])
	ts := strings.ToLower(strings.TrimSpace(toStr(r[4])))
	known := testPass[ts] || testFail[ts]
	if !known {
		errs[core.ReasonParse]++
	}
	h.OK = active == 1 && testPass[ts]
	until, ok := untilSeconds(r[5])
	if !ok {
		errs[core.ReasonParse]++
	}
	h.Until = until
	return h
}

// untilSeconds parses rateLimitedUntil: an RFC 3339 time (what 9router writes), or a Unix time in milliseconds
// (above 1e11) or seconds, as a number or a numeric string. Absent, empty or non-positive is 0 (no limit).
func untilSeconds(v any) (int64, bool) {
	if s, ok := v.(string); ok {
		s = strings.TrimSpace(s)
		if s == "" {
			return 0, true
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return max(t.Unix(), 0), true
		}
	}
	f, ok := toFloat(v)
	if !ok {
		return 0, false
	}
	if f > 1e11 {
		f /= 1000
	}
	switch {
	case f <= 0:
		return 0, true
	case f > maxUnix:
		return 0, false
	}
	return int64(f), true
}

const maxUnix = 253402300799 // 9999-12-31T23:59:59Z

// statusOK maps usageHistory.status to an outcome. 9router writes "ok" unless a caller sets another value, and
// reads a missing one as "ok"; "success" and an HTTP 2xx code are also success. Anything else is an error.
func statusOK(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case int64:
		return x >= 200 && x < 300
	case float64:
		return x >= 200 && x < 300
	}
	s := strings.ToLower(strings.TrimSpace(toStr(v)))
	switch s {
	case "", "ok", "success":
		return true
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 200 && n < 300
}

// providerLabel bounds the router's provider string: lowercase, label-safe, else "other"; empty is "unknown".
func providerLabel(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	switch {
	case p == "":
		return "unknown"
	case labelRe.MatchString(p):
		return p
	}
	return "other"
}

// modelLabel bounds the model string the same way as claude-code: lowercase, safe charset, ≤ 64 bytes.
func modelLabel(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	switch {
	case m == "":
		return "unknown"
	case modelRe.MatchString(m):
		return m
	}
	return "other"
}

func toStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

// toInt reads an integer column; NULL is 0. A real is truncated; text must be a number.
func toInt(v any) (int64, bool) {
	switch x := v.(type) {
	case nil:
		return 0, true
	case int64:
		return x, true
	case float64:
		if math.IsNaN(x) || math.Abs(x) > 1<<62 {
			return 0, false
		}
		return int64(x), true
	}
	f, ok := toFloat(v)
	if !ok || math.Abs(f) > 1<<62 {
		return 0, false
	}
	return int64(f), true
}

// toFloat reads a numeric column; NULL is 0.
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case nil:
		return 0, true
	case int64:
		return float64(x), true
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	case string, []byte:
		s := strings.TrimSpace(toStr(x))
		if s == "" {
			return 0, true
		}
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	return 0, false
}
