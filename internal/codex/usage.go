package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/safefs"
)

const (
	// UsageSourceName is the contract `source` value.
	UsageSourceName = "codex-usage"

	usageBase   = "https://chatgpt.com"
	usagePath   = "/backend-api/wham/usage"
	creditsPath = "/backend-api/wham/rate-limit-reset-credits" // read only; the spending path beside it is never called

	defaultUsageInterval = 3 * time.Minute
	defaultUsageBackoff  = 10 * time.Minute // after a 429 (floor in config.Floors)
	maxBody              = 1 << 20
	maxAuthFile          = 1 << 20
	netSpacing           = 5 * time.Second
)

// UsageSource is `codex-usage` (opt-in): the live quota windows, credits and reset-credit slots of one profile's
// account, read from the ChatGPT backend with the Codex CLI's own access token. The token is read from auth.json
// for one request and never refreshed (OpenAI rotates refresh tokens, so a refresh here could log the real CLI out),
// and auth.json is never written. When the token is expired the call is skipped and the last reading is served
// with its true observed time.
type UsageSource struct {
	Host, Alias, Account string
	Dir                  string // real CODEX_HOME path
	StaleAfter           time.Duration
	Interval             time.Duration
	Backoff429           time.Duration
	Version              string
	// Gate tells the profile's rollouts source when this source last produced a reading (see liveGate).
	Gate *liveGate

	// BaseURL and Transport override the vendor origin and transport. Tests only: they are not reachable from
	// the config file.
	BaseURL   string
	Transport http.RoundTripper
	Now       func() time.Time // the clock Restore judges by; nil = the wall clock
}

func (s *UsageSource) Name() string               { return UsageSourceName }
func (s *UsageSource) Harness() string            { return Harness }
func (s *UsageSource) Profile() string            { return s.Alias }
func (s *UsageSource) MinInterval() time.Duration { return s.Interval }
func (s *UsageSource) Network() bool              { return true }

// NetGroup keeps this endpoint's spacing and 429 backoff apart from Claude's and agy's.
func (s *UsageSource) NetGroup() string { return "chatgpt" }

// NetSpacing is short: two cheap GETs per account, and with the default 30 s gap ten accounts would take five
// minutes to read once.
func (s *UsageSource) NetSpacing() time.Duration { return netSpacing }

// liveGate is shared by a profile's codex-usage and codex-rollouts sources. Until codex-usage has a reading
// (and again once that reading is older than stale_after) rollouts serves the quota; while it has one, only
// codex-usage does. So a first-run failure of the live read never leaves a profile without quota, and no series
// is ever emitted by both.
type liveGate struct {
	mu sync.Mutex
	at time.Time
}

func (g *liveGate) set(t time.Time) {
	g.mu.Lock()
	if t.After(g.at) {
		g.at = t
	}
	g.mu.Unlock()
}

func (g *liveGate) last() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.at
}

// usageState is the persisted blob: the last good readings, served after a failure or a restart.
type usageState struct {
	Quotas map[string]*reading `json:"quotas,omitempty"`
	LastAt int64               `json:"last_at,omitempty"` // Unix ms of the last good quota read
	Resets *resetCredits       `json:"resets,omitempty"`
	C401   int                 `json:"c401,omitempty"`
	FP     string              `json:"fp,omitempty"` // auth.json fingerprint at the last usage 401
	// The reset-slot read has its own breaker: the usage read may keep working while this one is refused.
	CC401 int    `json:"cc401,omitempty"`
	CFP   string `json:"cfp,omitempty"`
}

type resetCredits struct {
	At        int64   `json:"at"` // Unix seconds of the read
	Available float64 `json:"n"`
	// Exp holds the expires_at of each open slot that had one, sorted, Unix seconds. A slot is no longer
	// usable once its time passes, so serving subtracts the past ones and moves the expiry to the next live one.
	Exp []int64 `json:"exp,omitempty"`
}

func (s *UsageSource) ident() ident { return ident{s.Host, s.Alias, s.Account} }

// serve builds the samples of the persisted readings as of now. Quota values older than stale_after are not
// served (rollouts takes over, see liveGate); reset slots past their expiry are dropped from the count.
func (s *UsageSource) serve(st *usageState, now time.Time) []core.Sample {
	var out []core.Sample
	if st.LastAt > 0 && now.Sub(time.UnixMilli(st.LastAt)) <= s.StaleAfter {
		out = serveQuotas(s.ident(), st.Quotas, s.StaleAfter, now)
	}
	if rc := st.Resets; rc != nil && now.Sub(time.Unix(rc.At, 0)) <= s.StaleAfter {
		avail, next := rc.Available, int64(0)
		for _, e := range rc.Exp {
			if e <= now.Unix() {
				avail--
			} else if next == 0 || e < next {
				next = e
			}
		}
		out = append(out, core.Sample{Name: "agent_quota_reset_credits_available", Labels: s.ident().labels(), Value: max(0, avail)})
		if next > 0 {
			out = append(out, core.Sample{Name: "agent_quota_reset_credits_next_expiry_timestamp_seconds",
				Labels: s.ident().labels(), Value: float64(next)})
		}
	}
	return out
}

// Restore serves the last good readings after a restart (staleness still applies in the core). Windows and slots
// are judged against the real time, not the time of the earlier read.
func (s *UsageSource) Restore(raw json.RawMessage) core.Result {
	var st usageState
	if json.Unmarshal(raw, &st) != nil || st.LastAt == 0 {
		return core.Result{}
	}
	at := time.UnixMilli(st.LastAt)
	if s.Gate != nil {
		s.Gate.set(at)
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	return core.Result{Samples: s.serve(&st, now), ObservedAt: at}
}

func (s *UsageSource) client() *http.Client {
	rt := s.Transport
	if rt == nil {
		// Direct connection only: an environment proxy would see the bearer token. No keep-alives: calls are
		// minutes apart.
		rt = &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second}
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (s *UsageSource) Collect(ctx context.Context, now time.Time, raw json.RawMessage) (core.Result, json.RawMessage, error) {
	var st usageState
	_ = json.Unmarshal(raw, &st)

	// fail serves the last good values (the core drops them when stale) alongside the error.
	fail := func(err error) (core.Result, json.RawMessage, error) {
		blob, _ := json.Marshal(st)
		res := core.Result{}
		if st.LastAt > 0 {
			if ss := s.serve(&st, now); len(ss) > 0 {
				res = core.Result{Samples: ss, ObservedAt: time.UnixMilli(st.LastAt)}
			}
		}
		return res, blob, err
	}

	a, err := readAuth(s.Dir)
	if err != nil {
		return fail(err)
	}
	if st.C401 >= 2 && a.fp == st.FP {
		return fail(core.Fail(core.ReasonAuthExpired)) // wait for the real client to refresh its login
	}
	if a.fp != st.FP {
		st.C401 = 0
	}
	if !a.exp.IsZero() && !a.exp.After(now) {
		return fail(core.Fail(core.ReasonAuthExpired)) // never refresh: the real client does that when it runs
	}

	status, body, err := s.get(ctx, a, usagePath)
	if err != nil {
		return fail(err)
	}
	switch {
	case status == http.StatusUnauthorized:
		st.C401++
		st.FP = a.fp
		return fail(core.Fail(core.ReasonAuthExpired))
	case status == http.StatusTooManyRequests:
		return fail(s.rateLimited())
	case status != http.StatusOK:
		return fail(core.Fail(core.ReasonUpstream))
	}
	quotas, perr := parseUsage(body, now)
	if perr != nil {
		return fail(core.Fail(core.ReasonParse))
	}
	st.Quotas, st.LastAt, st.C401, st.FP = quotas, now.UnixMilli(), 0, ""
	if s.Gate != nil {
		s.Gate.set(now)
	}

	// The reset slots are a second, independent read: its failure never costs the quota values just read.
	var credErr error
	errs := map[string]int{}
	if a.fp != st.CFP {
		st.CC401 = 0
	}
	if st.CC401 >= 2 {
		// two 401s on this read with this auth.json: wait for the real client to log in again
		status, body, err = http.StatusUnauthorized, nil, errSkipped
	} else {
		status, body, err = s.get(ctx, a, creditsPath)
	}
	switch {
	case err == errSkipped:
		credErr = core.Fail(core.ReasonAuthExpired)
	case err != nil:
		credErr = err
	case status == http.StatusTooManyRequests:
		credErr = s.rateLimited()
	case status == http.StatusUnauthorized:
		st.CC401++
		st.CFP = a.fp
		credErr = core.Fail(core.ReasonAuthExpired)
	case status != http.StatusOK:
		credErr = core.Fail(core.ReasonUpstream)
	default:
		if rc, ok := parseResetCredits(body, now); ok {
			st.Resets = rc
			st.CC401, st.CFP = 0, ""
		} else {
			credErr = core.Fail(core.ReasonParse)
		}
	}
	blob, _ := json.Marshal(st)
	res := core.Result{Samples: s.serve(&st, now), ObservedAt: now}
	var se *core.SourceError
	if errors.As(credErr, &se) {
		if se.Reason == core.ReasonRateLimited {
			return res, blob, credErr // fresh values and the backoff together
		}
		errs[se.Reason]++
		res.Errors = errs
	}
	return res, blob, nil
}

// errSkipped marks a call not made because of the reset-slot breaker.
var errSkipped = errors.New("skipped")

func (s *UsageSource) rateLimited() error {
	b := s.Backoff429
	if b <= 0 {
		b = defaultUsageBackoff
	}
	return &core.SourceError{Reason: core.ReasonRateLimited, Backoff: b}
}

// get makes one GET to the vendor and returns the status and a bounded body. Errors are bare contract reasons.
func (s *UsageSource) get(ctx context.Context, a auth, path string) (int, []byte, error) {
	base := s.BaseURL
	if base == "" {
		base = usageBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return 0, nil, core.Fail(core.ReasonOther)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if a.account != "" {
		req.Header.Set("ChatGPT-Account-Id", a.account)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "agent-quota-exporter/"+s.Version)
	resp, err := s.client().Do(req)
	req.Header.Del("Authorization")
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, nil, core.Fail(core.ReasonTimeout)
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return 0, nil, core.Fail(core.ReasonTimeout)
		}
		return 0, nil, core.Fail(core.ReasonUpstream) // err text can carry the URL; only the reason leaves
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if rerr != nil || len(body) > maxBody {
		return 0, nil, core.Fail(core.ReasonUpstream)
	}
	return resp.StatusCode, body, nil
}

// auth is what is taken from auth.json: the access token, the account id header value, the token's own expiry
// (JWT exp; zero when it has none) and a fingerprint of the file.
type auth struct {
	token, account, fp string
	exp                time.Time
}

var accountIDOK = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// readAuth reads auth.json for the access token only (read-only; nothing is ever written to it).
func readAuth(dir string) (auth, error) {
	if err := safefs.RealDir(dir); err != nil {
		return auth{}, err
	}
	f, fi, err := safefs.OpenRegular(dir + "/auth.json")
	if err != nil {
		return auth{}, err
	}
	defer f.Close()
	if fi.Size() > maxAuthFile {
		return auth{}, core.Fail(core.ReasonParse)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxAuthFile+1))
	if err != nil {
		return auth{}, core.Fail(core.ReasonOther)
	}
	var c struct {
		Tokens *struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	uerr := json.Unmarshal(b, &c)
	for i := range b {
		b[i] = 0
	}
	if uerr != nil {
		return auth{}, core.Fail(core.ReasonParse)
	}
	if c.Tokens == nil || !tokenShaped(c.Tokens.AccessToken) {
		return auth{}, core.Fail(core.ReasonAuthExpired) // an API-key login has no access token to use
	}
	a := auth{token: c.Tokens.AccessToken, exp: jwtExpiry(c.Tokens.AccessToken),
		fp: fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size())}
	if accountIDOK.MatchString(c.Tokens.AccountID) {
		a.account = c.Tokens.AccountID
	}
	return a, nil
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

// jwtExpiry returns the exp claim of a JWT access token, or zero when the token has none (an opaque token is then
// simply tried; a 401 is handled like any other).
func jwtExpiry(tok string) time.Time {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}
	}
	var c struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(payload, &c) != nil || !finite(c.Exp) || c.Exp <= 0 || c.Exp > latestReset {
		return time.Time{}
	}
	return time.Unix(int64(c.Exp), 0)
}

// whamWindow is one rate-limit window of /wham/usage. Field names rest on 9router's parser (v0.5.109); the
// aliases cover the spellings it accepts. Numbers are float64 so a vendor writing 300.0 does not fail the body.
type whamWindow struct {
	UsedPercent        *float64        `json:"used_percent"`
	PercentUsed        *float64        `json:"percent_used"`
	LimitWindowSeconds *float64        `json:"limit_window_seconds"`
	WindowMinutes      *float64        `json:"window_minutes"`
	ResetAt            json.RawMessage `json:"reset_at"`
	ResetsAt           json.RawMessage `json:"resets_at"`
	ResetAfterSeconds  *float64        `json:"reset_after_seconds"`
}

type whamLimit struct {
	Primary         *whamWindow `json:"primary"`
	PrimaryWindow   *whamWindow `json:"primary_window"`
	Secondary       *whamWindow `json:"secondary"`
	SecondaryWindow *whamWindow `json:"secondary_window"`
	RateLimit       *whamLimit  `json:"rate_limit"` // an entry may wrap its windows
}

func (l *whamLimit) windows() (primary, secondary *whamWindow) {
	if l == nil {
		return nil, nil
	}
	if l.RateLimit != nil && l.PrimaryWindow == nil && l.Primary == nil && l.SecondaryWindow == nil && l.Secondary == nil {
		return l.RateLimit.windows()
	}
	return pick(l.PrimaryWindow, l.Primary), pick(l.SecondaryWindow, l.Secondary)
}

func pick(a, b *whamWindow) *whamWindow {
	if a != nil {
		return a
	}
	return b
}

type whamBody struct {
	whamLimit                               // fields at the top level, if the windows are not under rate_limit
	RateLimits   json.RawMessage            `json:"rate_limits"` // an object like rate_limit (9router also tries it)
	CodeReview   *whamLimit                 `json:"code_review_rate_limit"`
	ByLimitID    map[string]json.RawMessage `json:"rate_limits_by_limit_id"`
	Additional   []json.RawMessage          `json:"additional_rate_limits"`
	LimitReached *bool                      `json:"limit_reached"`
	Credits      *rlCredits                 `json:"credits"`
}

// toReading converts one limit into the reading the rollouts source stores, so mapping to window and scope
// labels is the one function newReading. id nil is the main limit (scope "all").
func toReading(l *whamLimit, id *string, at time.Time) *reading {
	p, sec := l.windows()
	rl := &rateLimits{LimitID: id}
	// Codex's own naming (9router: session / weekly) gives the lengths when the body states none.
	rl.Primary = rlOf(p, 300, at)
	rl.Secondary = rlOf(sec, 10080, at)
	if rl.Primary == nil && rl.Secondary == nil {
		return nil
	}
	return newReading(rl, at)
}

func rlOf(w *whamWindow, defaultMinutes float64, at time.Time) *rlWindow {
	if w == nil {
		return nil
	}
	used := w.UsedPercent
	if used == nil {
		used = w.PercentUsed
	}
	if used == nil {
		return nil // absent, never zero
	}
	out := &rlWindow{UsedPercent: used}
	switch {
	case w.LimitWindowSeconds != nil && finite(*w.LimitWindowSeconds):
		m := *w.LimitWindowSeconds / 60
		out.WindowMinutes = &m
	case w.WindowMinutes != nil:
		out.WindowMinutes = w.WindowMinutes
	default:
		m := defaultMinutes
		out.WindowMinutes = &m
	}
	if t, ok := unixOf(cmpRaw(w.ResetAt, w.ResetsAt)); ok {
		v := float64(t)
		out.ResetsAt = &v
	} else if w.ResetAfterSeconds != nil && finite(*w.ResetAfterSeconds) && *w.ResetAfterSeconds >= 0 {
		v := float64(at.Unix()) + math.Round(*w.ResetAfterSeconds)
		out.ResetsAt = &v
	}
	return out
}

func cmpRaw(a, b json.RawMessage) json.RawMessage {
	if len(a) > 0 && string(a) != "null" {
		return a
	}
	return b
}

// unixOf reads a time the vendor may write as Unix seconds, Unix milliseconds or an RFC 3339 string.
func unixOf(raw json.RawMessage) (int64, bool) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return 0, false
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		if !finite(v) || v <= 0 {
			return 0, false
		}
		if v >= 1e12 {
			v /= 1000
		}
		if v > latestReset {
			return 0, false
		}
		return int64(v), true
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.Unix(), true
	}
	return 0, false
}

// parseUsage maps a /wham/usage body to readings by scope: the main limit is scope "all", the rest by the same
// scopeLabel the rollouts use for limit_id (code review: "code_review"). The first reading of a scope wins.
func parseUsage(body []byte, at time.Time) (map[string]*reading, error) {
	var b whamBody
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, err
	}
	out := map[string]*reading{}
	add := func(l *whamLimit, id *string) {
		if l == nil || len(out) >= maxScopes {
			return
		}
		r := toReading(l, id, at)
		if r == nil {
			return
		}
		if _, dup := out[r.Scope]; !dup {
			out[r.Scope] = r
		}
	}
	// The main limit sits under rate_limit; 9router also tries rate_limits and the top level.
	var alt whamLimit
	_ = json.Unmarshal(b.RateLimits, &alt) // a non-object (or absent) value leaves it empty
	main := &b.whamLimit
	for _, c := range []*whamLimit{b.RateLimit, &alt, &b.whamLimit} {
		if p, sec := c.windows(); p != nil || sec != nil {
			main = c
			break
		}
	}
	add(main, nil)
	review := "code_review"
	add(b.CodeReview, &review)
	ids := make([]string, 0, len(b.ByLimitID))
	for id := range b.ByLimitID {
		ids = append(ids, id)
	}
	sort.Strings(ids) // which entries fit under maxScopes must not depend on map order
	for _, id := range ids {
		var l whamLimit
		if json.Unmarshal(b.ByLimitID[id], &l) == nil {
			add(&l, &id)
		}
	}
	for _, raw := range b.Additional {
		var e struct {
			whamLimit
			LimitName      string `json:"limit_name"`
			MeteredFeature string `json:"metered_feature"`
			ID             string `json:"id"`
		}
		if json.Unmarshal(raw, &e) != nil {
			continue
		}
		id := firstNonEmpty(e.MeteredFeature, e.LimitName, e.ID)
		add(&e.whamLimit, &id)
	}
	if len(out) == 0 {
		return nil, errors.New("no quota windows")
	}
	// Credits belong to the main limit, as in the rollouts' rate_limits.
	if m := out["all"]; m != nil && b.Credits != nil {
		longest := int64(0)
		for _, w := range m.Windows {
			longest = max(longest, w.Reset)
		}
		m.Credits = newCredits(b.Credits, longest, at)
	}
	return out, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// parseResetCredits reads /wham/rate-limit-reset-credits: available_count and credits[].{status, expires_at}.
// ok is false when the body states neither a count nor a list. Expiries are kept for credits that are not spent and
// not already past; with none (or no expires_at) there is no expiry series, never 0.
func parseResetCredits(body []byte, now time.Time) (*resetCredits, bool) {
	var b struct {
		AvailableCount *float64 `json:"available_count"`
		Credits        []struct {
			Status    string          `json:"status"`
			ExpiresAt json.RawMessage `json:"expires_at"`
		} `json:"credits"`
	}
	if json.Unmarshal(body, &b) != nil || (b.AvailableCount == nil && b.Credits == nil) {
		return nil, false
	}
	rc := &resetCredits{At: now.Unix()}
	open := 0.0
	for _, c := range b.Credits {
		switch strings.ToLower(c.Status) {
		case "consumed", "redeemed", "used", "expired", "revoked", "cancelled", "canceled":
			continue
		}
		if t, ok := unixOf(c.ExpiresAt); ok && t <= now.Unix() {
			continue // already past: not usable whatever its status says
		}
		open++
		if t, ok := unixOf(c.ExpiresAt); ok {
			rc.Exp = append(rc.Exp, t)
		}
	}
	sort.Slice(rc.Exp, func(i, j int) bool { return rc.Exp[i] < rc.Exp[j] })
	switch {
	case b.AvailableCount != nil && finite(*b.AvailableCount):
		rc.Available = max(0, *b.AvailableCount)
	case b.AvailableCount == nil:
		rc.Available = open
	default:
		return nil, false
	}
	return rc, true
}
