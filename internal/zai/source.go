package zai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

const (
	// SourceName is the contract `source` value.
	SourceName = "zai-usage"

	apiBase   = "https://api.z.ai"
	quotaPath = "/api/monitor/usage/quota/limit"

	DefaultInterval   = 10 * time.Minute
	DefaultBackoff429 = 30 * time.Minute
	maxBody           = 1 << 20
	netSpacing        = 5 * time.Second
)

// Source is `zai-usage` (opt-in): the Coding Plan quota windows of one z.ai account, read from the key a local
// harness already holds. The key is read from the holder's file for one request, never stored, logged or refreshed,
// and the holder's files are never written. Two profiles holding the same key are read once (see Peers).
type Source struct {
	Host, Alias, Account string
	Dir                  string // the holder's directory, meaning per KeySource
	KeySource            string // openclaw | opencode | zai-helper
	Version              string
	Interval             time.Duration
	Backoff429           time.Duration
	StaleAfter           time.Duration
	Peers                *Peers

	// BaseURL, Transport and Now override the vendor origin, transport and clock. Tests only: they are not
	// reachable from the config file.
	BaseURL   string
	Transport http.RoundTripper
	Now       func() time.Time
}

func (s *Source) Name() string               { return SourceName }
func (s *Source) Harness() string            { return Harness }
func (s *Source) Profile() string            { return s.Alias }
func (s *Source) MinInterval() time.Duration { return s.Interval }
func (s *Source) Network() bool              { return true }

// NetGroup keeps a z.ai 429 apart from every other vendor's calls.
func (s *Source) NetGroup() string { return "zai" }

// NetSpacing is short: one cheap GET per account.
func (s *Source) NetSpacing() time.Duration { return netSpacing }

func (s *Source) ident() ident { return ident{s.Host, s.Alias, s.Account} }

func (s *Source) limit() time.Duration { return core.FreshnessLimit(s, s.StaleAfter) }

// readKey reads this profile's key. The caller zeroes it.
func (s *Source) readKey() (key, error) {
	rd, ok := readerFor(s.KeySource)
	if !ok {
		return key{}, core.Fail(core.ReasonOther)
	}
	return rd(s.Dir)
}

// Restore serves the last good reading after a restart (staleness still applies in the core), unless this
// profile defers to a peer holding the same key.
func (s *Source) Restore(raw json.RawMessage) core.Result {
	var st state
	if json.Unmarshal(raw, &st) != nil || st.LastAt == 0 {
		return core.Result{}
	}
	k, err := s.readKey()
	if err != nil {
		return core.Result{} // ownership cannot be verified: a peer may be serving this account
	}
	owns := s.Peers == nil || s.Peers.owns(s, k.value, false)
	k.Zero()
	if !owns {
		return core.Result{}
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	return core.Result{Samples: serve(s.ident(), &st, s.limit(), now), ObservedAt: time.UnixMilli(st.LastAt)}
}

func (s *Source) client() *http.Client {
	rt := s.Transport
	if rt == nil {
		// Direct connection only: an environment proxy would see the key. No keep-alives: calls are minutes apart.
		rt = &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second}
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (s *Source) Collect(ctx context.Context, now time.Time, raw json.RawMessage) (core.Result, json.RawMessage, error) {
	var st state
	_ = json.Unmarshal(raw, &st)

	// fail serves the last good values (the core drops them when stale) alongside the error.
	fail := func(err error) (core.Result, json.RawMessage, error) {
		blob, _ := json.Marshal(st)
		res := core.Result{}
		if ss := serve(s.ident(), &st, s.limit(), now); len(ss) > 0 {
			res = core.Result{Samples: ss, ObservedAt: time.UnixMilli(st.LastAt)}
		}
		return res, blob, err
	}

	k, err := s.readKey()
	if err != nil {
		// Without the key, ownership cannot be verified: serve nothing, and drop what an earlier poll cached.
		blob, _ := json.Marshal(st)
		var se *core.SourceError
		if errors.As(err, &se) {
			err = &core.SourceError{Reason: se.Reason, DropCache: true}
		}
		return core.Result{}, blob, err
	}
	defer k.Zero()
	if s.Peers != nil && !s.Peers.owns(s, k.value, true) {
		return core.Result{}, raw, nil // the owner of this key reads it; nothing to report here
	}
	if st.C401 >= 2 && k.fp == st.FP {
		return fail(core.Fail(core.ReasonAuthExpired)) // no refresh exists: wait for the holder to write a new key
	}
	if k.fp != st.FP {
		st.C401 = 0
	}

	status, body, err := s.get(ctx, &k)
	if err != nil {
		return fail(err)
	}
	authFailed := func() (core.Result, json.RawMessage, error) {
		st.C401++
		st.FP = k.fp
		return fail(core.Fail(core.ReasonAuthExpired))
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return authFailed()
	case status == http.StatusTooManyRequests:
		return fail(s.rateLimited())
	case status != http.StatusOK:
		return fail(core.Fail(core.ReasonUpstream))
	}
	p, reason := parseQuota(body)
	switch reason {
	case "":
	case core.ReasonAuthExpired:
		return authFailed()
	case core.ReasonRateLimited:
		return fail(s.rateLimited())
	default:
		return fail(core.Fail(reason))
	}
	st.Reading, st.LastAt, st.C401, st.FP = p.Reading, now.UnixMilli(), 0, ""
	blob, _ := json.Marshal(st)
	res := core.Result{Samples: serve(s.ident(), &st, s.limit(), now), ObservedAt: now}
	if p.Unknown > 0 {
		res.Errors = map[string]int{core.ReasonParse: p.Unknown}
	}
	return res, blob, nil
}

func (s *Source) rateLimited() error {
	b := s.Backoff429
	if b <= 0 {
		b = DefaultBackoff429
	}
	return &core.SourceError{Reason: core.ReasonRateLimited, Backoff: b}
}

// get makes the one GET and returns the status and a bounded body. Errors are bare contract reasons. The key is
// the raw Authorization value (no Bearer), set from a local and deleted after the call.
func (s *Source) get(ctx context.Context, k *key) (int, []byte, error) {
	base := s.BaseURL
	if base == "" {
		base = apiBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+quotaPath, nil)
	if err != nil {
		return 0, nil, core.Fail(core.ReasonOther)
	}
	req.Header.Set("Authorization", string(k.value))
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
