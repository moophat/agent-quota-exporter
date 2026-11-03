package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

const (
	usageBase  = "https://api.anthropic.com"
	usagePath  = "/api/oauth/usage"
	betaHeader = "oauth-2025-04-20"
	maxBody    = 1 << 20
)

// QuotaCache shares one vendor answer between profiles that hold the same account, so an account is asked at
// most once per interval however many profiles carry it (specs/sources.md). Profiles with an unknown account id
// never share.
type QuotaCache struct {
	mu sync.Mutex
	m  map[string]quotaEntry
}

type quotaEntry struct {
	at time.Time
	q  parsedQuota
}

func NewQuotaCache() *QuotaCache { return &QuotaCache{m: map[string]quotaEntry{}} }

type qWindow struct {
	Window string  `json:"w"`
	Scope  string  `json:"s"`
	Ratio  float64 `json:"r"`
	Reset  int64   `json:"t,omitempty"`
}

type qLimit struct {
	Window   string `json:"w"`
	Scope    string `json:"s"`
	Severity string `json:"v"`
}

type parsedQuota struct {
	Windows []qWindow `json:"windows"`
	Limits  []qLimit  `json:"limits"`
}

type oauthState struct {
	C401   int          `json:"c401,omitempty"`
	FP     string       `json:"fp,omitempty"` // credential file fingerprint at the last 401
	Last   *parsedQuota `json:"last,omitempty"`
	LastAt int64        `json:"last_at,omitempty"`
	Acct   string       `json:"acct,omitempty"` // account label the last values belong to
}

// OAuthUsageSource is `claude-oauth-usage` (opt-in): the vendor's quota windows for one profile's account.
// It is the only code that ever holds an access token, for one request. It never refreshes a token.
type OAuthUsageSource struct {
	P          *Profile
	Interval   time.Duration
	Backoff429 time.Duration
	Version    string
	Cache      *QuotaCache

	// Endpoint and Transport override the vendor URL and transport. Tests only: they are not reachable from
	// the config file.
	Endpoint  string
	Transport http.RoundTripper
}

func (s *OAuthUsageSource) Name() string               { return "claude-oauth-usage" }
func (s *OAuthUsageSource) Harness() string            { return Harness }
func (s *OAuthUsageSource) Profile() string            { return s.P.Alias }
func (s *OAuthUsageSource) MinInterval() time.Duration { return s.Interval }
func (s *OAuthUsageSource) Network() bool              { return true }

// Restore serves the last good answer after a restart (staleness still applies in the core).
func (s *OAuthUsageSource) Restore(raw json.RawMessage) core.Result {
	var st oauthState
	if json.Unmarshal(raw, &st) != nil || st.Last == nil || st.LastAt == 0 {
		return core.Result{}
	}
	if label, _ := s.P.Account(); label != st.Acct {
		return core.Result{} // the profile now holds another login
	}
	return core.Result{Samples: s.samples(st.Acct, *st.Last, time.Unix(st.LastAt, 0)), ObservedAt: time.Unix(st.LastAt, 0)}
}

func (s *OAuthUsageSource) client() *http.Client {
	rt := s.Transport
	if rt == nil {
		// Direct connection only: an environment proxy would see the bearer token.
		// No keep-alives: calls are 15 min apart, and a pooled idle connection per call would only pile up.
		rt = &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second}
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (s *OAuthUsageSource) Collect(ctx context.Context, now time.Time, raw json.RawMessage) (core.Result, json.RawMessage, error) {
	var st oauthState
	_ = json.Unmarshal(raw, &st)
	_ = s.P.ResolveAccount() // re-read on change: a re-login must not reuse another account's answer
	label, uuid := s.P.Account()

	// fail returns the error while still serving the last good values (the core drops them when stale).
	fail := func(err error) (core.Result, json.RawMessage, error) {
		blob, _ := json.Marshal(st)
		res := core.Result{}
		if st.Last != nil && st.LastAt > 0 && st.Acct == label { // never another login's quota under this profile
			res = core.Result{Samples: s.samples(st.Acct, *st.Last, time.Unix(st.LastAt, 0)), ObservedAt: time.Unix(st.LastAt, 0)}
		}
		return res, blob, err
	}
	ok := func(q parsedQuota, at time.Time) (core.Result, json.RawMessage, error) {
		st.Last, st.LastAt, st.Acct, st.C401, st.FP = &q, at.Unix(), label, 0, ""
		blob, _ := json.Marshal(st)
		return core.Result{Samples: s.samples(label, q, at), ObservedAt: at}, blob, nil
	}

	if uuid != "" && s.Cache != nil {
		s.Cache.mu.Lock()
		e, hit := s.Cache.m[uuid]
		s.Cache.mu.Unlock()
		if hit && now.Sub(e.at) < s.Interval {
			return ok(e.q, e.at)
		}
	}

	tok, exp, fp, err := readToken(s.P.Dir)
	if err != nil {
		return fail(err)
	}
	if st.C401 >= 2 && fp == st.FP {
		return fail(core.Fail(core.ReasonAuthExpired)) // wait for the real client to refresh its login
	}
	if fp != st.FP {
		st.C401 = 0
	}
	if !exp.IsZero() && !exp.After(now) {
		return fail(core.Fail(core.ReasonAuthExpired)) // never refresh: an idle client's token stays expired
	}

	base := s.Endpoint
	if base == "" {
		base = usageBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+usagePath, nil)
	if err != nil {
		return fail(core.Fail(core.ReasonOther))
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	tok = ""
	req.Header.Set("anthropic-beta", betaHeader)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "agent-quota-exporter/"+s.Version)
	resp, err := s.client().Do(req)
	req.Header.Del("Authorization")
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return fail(core.Fail(core.ReasonTimeout))
		}
		return fail(core.Fail(core.ReasonUpstream))
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		st.C401++
		st.FP = fp
		return fail(core.Fail(core.ReasonAuthExpired))
	case resp.StatusCode == http.StatusTooManyRequests:
		backoff := s.Backoff429
		if backoff <= 0 {
			backoff = time.Hour
		}
		return fail(&core.SourceError{Reason: core.ReasonRateLimited, Backoff: backoff})
	case resp.StatusCode != http.StatusOK:
		return fail(core.Fail(core.ReasonUpstream))
	case rerr != nil || len(body) > maxBody:
		return fail(core.Fail(core.ReasonUpstream))
	}
	q, perr := parseUsage(body)
	if perr != nil {
		return fail(core.Fail(core.ReasonParse))
	}
	if uuid != "" && s.Cache != nil {
		s.Cache.mu.Lock()
		s.Cache.m[uuid] = quotaEntry{at: now, q: q}
		s.Cache.mu.Unlock()
	}
	return ok(q, now)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// readToken reads the access token, its expiry and a fingerprint of the credential file. The file is read again
// once if it changed during the read (the real client rewrites it on refresh), so a half-written token is not
// sent; a token that is not token-shaped is never sent.
func readToken(dir string) (string, time.Time, string, error) {
	if err := realDir(dir); err != nil {
		return "", time.Time{}, "", err
	}
	path := filepath.Join(dir, ".credentials.json")
	for attempt := 0; attempt < 2; attempt++ {
		before, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", time.Time{}, "", core.Fail(core.ReasonAuthExpired)
			}
			return "", time.Time{}, "", core.Fail(core.ErrReason(err))
		}
		b, err := readSmall(path, 1<<20)
		if err != nil {
			return "", time.Time{}, "", err
		}
		after, err := os.Lstat(path)
		if err != nil || !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
			wipe(b)
			continue
		}
		var c struct {
			OAuth *struct {
				AccessToken string `json:"accessToken"`
				ExpiresAt   int64  `json:"expiresAt"`
			} `json:"claudeAiOauth"`
		}
		uerr := json.Unmarshal(b, &c)
		wipe(b)
		if uerr != nil || c.OAuth == nil || !tokenShaped(c.OAuth.AccessToken) {
			return "", time.Time{}, "", core.Fail(core.ReasonAuthExpired)
		}
		var exp time.Time
		if c.OAuth.ExpiresAt > 0 {
			exp = time.UnixMilli(c.OAuth.ExpiresAt)
		}
		fp := fmt.Sprintf("%d:%d", after.ModTime().UnixNano(), c.OAuth.ExpiresAt)
		return c.OAuth.AccessToken, exp, fp, nil
	}
	return "", time.Time{}, "", core.Fail(core.ReasonOther)
}

func tokenShaped(t string) bool {
	if len(t) < 20 || len(t) > 8192 {
		return false
	}
	for i := 0; i < len(t); i++ {
		if t[i] <= ' ' || t[i] > '~' {
			return false
		}
	}
	return true
}

var scopeSafe = regexp.MustCompile(`[^a-z0-9_-]+`)

// windowOf maps a response key to the contract window and scope. Only five_hour* and seven_day* keys are quota
// windows; anything else (credits, extra usage, future keys) is ignored rather than invented as a window.
func windowOf(key string) (string, string, bool) {
	key = strings.ToLower(key)
	for _, w := range []struct{ prefix, window string }{{"five_hour", "5h"}, {"seven_day", "7d"}} {
		if key == w.prefix {
			return w.window, "all", true
		}
		if rest, ok := strings.CutPrefix(key, w.prefix+"_"); ok {
			if sc := strings.Trim(scopeSafe.ReplaceAllString(rest, "_"), "_"); sc != "" && len(sc) <= 48 {
				return w.window, sc, true
			}
		}
	}
	return "", "", false
}

// limitWindow maps a limits[].kind to window and scope: session → 5h/all, weekly_all → 7d/all, weekly_X → 7d/X.
func limitWindow(kind string) (string, string, bool) {
	kind = strings.ToLower(kind)
	switch {
	case kind == "session":
		return "5h", "all", true
	case kind == "weekly_all" || kind == "weekly":
		return "7d", "all", true
	case strings.HasPrefix(kind, "weekly_"):
		if sc := strings.Trim(scopeSafe.ReplaceAllString(kind[len("weekly_"):], "_"), "_"); sc != "" && len(sc) <= 48 {
			return "7d", sc, true
		}
	}
	return "", "", false
}

var severitySafe = regexp.MustCompile(`^[a-z_]{1,32}$`)

func parseUsage(body []byte) (parsedQuota, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return parsedQuota{}, err
	}
	var q parsedQuota
	for k, v := range top {
		w, sc, ok := windowOf(k)
		if !ok {
			continue
		}
		var o *struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    *string  `json:"resets_at"`
		}
		if json.Unmarshal(v, &o) != nil || o == nil || o.Utilization == nil {
			continue // null window or null utilization: absent, never zero
		}
		qw := qWindow{Window: w, Scope: sc, Ratio: *o.Utilization / 100}
		if o.ResetsAt != nil {
			if t, err := time.Parse(time.RFC3339Nano, *o.ResetsAt); err == nil {
				qw.Reset = t.Unix()
			}
		}
		q.Windows = append(q.Windows, qw)
	}
	if lr, ok := top["limits"]; ok {
		var ls []struct {
			Kind     string `json:"kind"`
			Severity string `json:"severity"`
			IsActive bool   `json:"is_active"`
		}
		if json.Unmarshal(lr, &ls) == nil {
			seen := map[string]bool{}
			for _, l := range ls {
				w, sc, ok := limitWindow(l.Kind)
				if !l.IsActive || !ok {
					continue
				}
				sev := strings.ToLower(l.Severity)
				if !severitySafe.MatchString(sev) {
					sev = "other"
				}
				k := w + "/" + sc
				if seen[k] {
					continue // one current severity per window and scope
				}
				seen[k] = true
				q.Limits = append(q.Limits, qLimit{w, sc, sev})
			}
		}
	}
	if len(q.Windows) == 0 && len(q.Limits) == 0 {
		return q, errors.New("no quota windows")
	}
	sort.Slice(q.Windows, func(i, j int) bool {
		return q.Windows[i].Window+q.Windows[i].Scope < q.Windows[j].Window+q.Windows[j].Scope
	})
	return q, nil
}

func (s *OAuthUsageSource) samples(account string, q parsedQuota, at time.Time) []core.Sample {
	pl := core.Label{Name: "provider", Value: Provider}
	var out []core.Sample
	for _, w := range q.Windows {
		ls := s.P.labels(account, pl, core.Label{Name: "window", Value: w.Window}, core.Label{Name: "scope", Value: w.Scope})
		out = append(out, core.Sample{Name: "agent_quota_utilization_ratio", Labels: ls, Value: w.Ratio})
		if w.Reset > 0 {
			out = append(out, core.Sample{Name: "agent_quota_reset_timestamp_seconds", Labels: ls, Value: float64(w.Reset)})
		}
	}
	for _, l := range q.Limits {
		out = append(out, core.Sample{Name: "agent_quota_limit_info", Labels: s.P.labels(account, pl,
			core.Label{Name: "window", Value: l.Window}, core.Label{Name: "scope", Value: l.Scope},
			core.Label{Name: "severity", Value: l.Severity}), Value: 1})
	}
	out = append(out, core.Sample{Name: "agent_quota_observed_timestamp_seconds", Labels: s.P.labels(account, pl), Value: float64(at.Unix())})
	return out
}
