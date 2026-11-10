package zai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

type recorded struct {
	method, host, path, auth, accept, ua string
	hasBody                              bool
}

// fakeRT is the HTTPS transport: it records every request (the Authorization header is copied before the source
// deletes it) and answers from handler.
type fakeRT struct {
	mu      sync.Mutex
	reqs    []recorded
	handler func(*http.Request) (*http.Response, error)
}

func (f *fakeRT) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{r.Method, r.URL.Host, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Accept"),
		r.Header.Get("User-Agent"), r.Body != nil && r.Body != http.NoBody})
	f.mu.Unlock()
	return f.handler(r)
}

func (f *fakeRT) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.reqs) }

func respond(status int, body []byte) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}}, nil
	}
}

type fixt struct {
	src *Source
	rt  *fakeRT
	now time.Time
}

// holderFor writes the fixture of key source ks into a temp dir.
func holderFor(t *testing.T, ks, key string) string {
	t.Helper()
	switch ks {
	case "openclaw":
		return holderDir(t, "openclaw.json", `{"models":{"providers":{"zai":{"apiKey":"`+key+`"}}}}`)
	case "zai-helper":
		return holderDir(t, "config.yaml", "api_key: "+key+"\n")
	case "opencode":
		return holderDir(t, "auth.json", `{"zai-coding-plan":{"type":"api","key":"`+key+`"}}`)
	}
	t.Fatal(ks)
	return ""
}

func newFixt(t *testing.T, ks, alias string, h func(*http.Request) (*http.Response, error)) *fixt {
	t.Helper()
	rt := &fakeRT{handler: h}
	f := &fixt{rt: rt, now: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)}
	f.src = &Source{Host: "node-a", Alias: alias, Account: "zai-main", Dir: holderFor(t, ks, canaryKey), KeySource: ks, Version: "test",
		Interval: 10 * time.Minute, Backoff429: 30 * time.Minute, StaleAfter: 45 * time.Minute, Transport: rt,
		Now: func() time.Time { return f.now }}
	return f
}

func (f *fixt) collect(t *testing.T, st json.RawMessage) (core.Result, json.RawMessage, error) {
	t.Helper()
	return f.src.Collect(context.Background(), f.now, st)
}

func globalBody(t *testing.T) []byte { return fixtureBytes(t, "quota-limit-global.json") }

func TestSourceIdentity(t *testing.T) {
	s := &Source{Alias: "z", Interval: 10 * time.Minute}
	if s.Name() != "zai-usage" || s.Harness() != "zai" || s.Profile() != "z" || !s.Network() || s.NetGroup() != "zai" ||
		s.NetSpacing() != 5*time.Second || s.MinInterval() != 10*time.Minute {
		t.Fatal("identity")
	}
}

func TestHappyPathPerKeySource(t *testing.T) {
	for _, ks := range []string{"openclaw", "opencode", "zai-helper"} {
		t.Run(ks, func(t *testing.T) {
			f := newFixt(t, ks, "z", nil)
			f.rt.handler = respond(200, globalBody(t))
			res, blob, err := f.collect(t, nil)
			if err != nil {
				t.Fatal(err)
			}
			if f.rt.count() != 1 {
				t.Fatalf("%d requests", f.rt.count())
			}
			r := f.rt.reqs[0]
			if r.method != "GET" || r.host != "api.z.ai" || r.path != "/api/monitor/usage/quota/limit" || r.hasBody {
				t.Fatalf("request %+v", r)
			}
			if r.auth != canaryKey || strings.Contains(r.auth, "Bearer") {
				t.Fatal("Authorization must be the raw key")
			}
			if r.accept != "application/json" || r.ua != "agent-quota-exporter/test" {
				t.Fatalf("headers %+v", r)
			}
			got := byKey(res.Samples)
			if got["agent_quota_utilization_ratio|7d|all"] != 0 || got["agent_quota_limit_calls|monthly|tool_calls"] != 100 ||
				got["agent_quota_observed_timestamp_seconds||"] != float64(f.now.Unix()) {
				t.Fatalf("samples %v", got)
			}
			if !res.ObservedAt.Equal(f.now) || len(res.Errors) != 0 || len(blob) == 0 {
				t.Fatalf("result %+v", res)
			}
			for _, s := range res.Samples {
				for _, l := range s.Labels {
					if l.Name == "account" && l.Value != "zai-main" {
						t.Fatal("account label must be the configured one")
					}
				}
			}
		})
	}
}

// Only api.z.ai is ever requested: the key cannot go anywhere else.
func TestOnlyOneHost(t *testing.T) {
	f := newFixt(t, "openclaw", "z", respond(200, globalBody(t)))
	for i := 0; i < 3; i++ {
		f.collect(t, nil)
	}
	for _, r := range f.rt.reqs {
		if r.host != "api.z.ai" {
			t.Fatalf("requested %s", r.host)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

func TestFailureRows(t *testing.T) {
	big := bytes.Repeat([]byte("x"), maxBody+10)
	rows := map[string]struct {
		h      func(*http.Request) (*http.Response, error)
		reason string
	}{
		"401":              {respond(401, nil), core.ReasonAuthExpired},
		"403":              {respond(403, nil), core.ReasonAuthExpired},
		"200 code 1000":    {respond(200, []byte(`{"code":1000,"msg":"x","success":false}`)), core.ReasonAuthExpired},
		"200 code 1001":    {respond(200, fixtureBytes(t, "quota-limit-authfail.json")), core.ReasonAuthExpired},
		"200 code 1004":    {respond(200, []byte(`{"code":1004,"success":false}`)), core.ReasonAuthExpired},
		"429":              {respond(429, nil), core.ReasonRateLimited},
		"200 code 1302":    {respond(200, []byte(`{"code":1302,"success":false}`)), core.ReasonRateLimited},
		"200 code 1305":    {respond(200, []byte(`{"code":1305,"success":false}`)), core.ReasonRateLimited},
		"200 other code":   {respond(200, []byte(`{"code":500,"success":false}`)), core.ReasonUpstream},
		"500":              {respond(500, []byte("oops")), core.ReasonUpstream},
		"404":              {respond(404, nil), core.ReasonUpstream},
		"redirect":         {respond(302, nil), core.ReasonUpstream},
		"body too large":   {respond(200, big), core.ReasonUpstream},
		"transport error":  {func(*http.Request) (*http.Response, error) { return nil, errors.New("dial tcp 1.2.3.4: refused") }, core.ReasonUpstream},
		"timeout":          {func(*http.Request) (*http.Response, error) { return nil, timeoutErr{} }, core.ReasonTimeout},
		"deadline":         {func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded }, core.ReasonTimeout},
		"not json":         {respond(200, []byte("<html>")), core.ReasonParse},
		"empty envelope":   {respond(200, []byte(`{}`)), core.ReasonParse},
		"no limits":        {respond(200, fixtureBytes(t, "quota-limit-nolimits.json")), core.ReasonParse},
		"success no limit": {respond(200, []byte(`{"success":true,"data":{"limits":[]}}`)), core.ReasonParse},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			f := newFixt(t, "openclaw", "z", row.h)
			res, _, err := f.collect(t, nil)
			var se *core.SourceError
			if !errors.As(err, &se) || se.Reason != row.reason {
				t.Fatalf("err %v, want %s", err, row.reason)
			}
			if len(res.Samples) != 0 {
				t.Fatalf("samples without a good reading: %d", len(res.Samples))
			}
			if row.reason == core.ReasonRateLimited && se.Backoff != 30*time.Minute {
				t.Fatalf("backoff %v", se.Backoff)
			}
			if row.reason != core.ReasonRateLimited && se.Backoff != 0 {
				t.Fatalf("backoff %v on %s", se.Backoff, row.reason)
			}
		})
	}
}

func TestBackoffOverride(t *testing.T) {
	f := newFixt(t, "openclaw", "z", respond(429, nil))
	f.src.Backoff429 = 45 * time.Minute
	_, _, err := f.collect(t, nil)
	var se *core.SourceError
	if !errors.As(err, &se) || se.Backoff != 45*time.Minute {
		t.Fatalf("%v", err)
	}
	f.src.Backoff429 = 0
	_, _, err = f.collect(t, nil)
	if !errors.As(err, &se) || se.Backoff != 30*time.Minute {
		t.Fatal("default backoff")
	}
}

func TestHolderFailures(t *testing.T) {
	cases := map[string]struct {
		prep   func(t *testing.T, f *fixt)
		reason string
	}{
		"dir missing": {func(t *testing.T, f *fixt) { f.src.Dir = filepath.Join(t.TempDir(), "absent") }, core.ReasonNotFound},
		"key missing": {func(t *testing.T, f *fixt) { f.src.Dir = t.TempDir() }, core.ReasonNotFound},
		"dir is link": {func(t *testing.T, f *fixt) {
			l := filepath.Join(t.TempDir(), "l")
			if err := os.Symlink(f.src.Dir, l); err != nil {
				t.Fatal(err)
			}
			f.src.Dir = l
		}, core.ReasonPermission},
		"malformed": {func(t *testing.T, f *fixt) {
			os.WriteFile(filepath.Join(f.src.Dir, "openclaw.json"), []byte(`{"models":`), 0o600)
		}, core.ReasonParse},
		"secret ref": {func(t *testing.T, f *fixt) {
			os.WriteFile(filepath.Join(f.src.Dir, "openclaw.json"), []byte(`{"models":{"providers":{"zai":{"apiKey":{"source":"env"}}}}}`), 0o600)
		}, core.ReasonParse},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixt(t, "openclaw", "z", respond(200, globalBody(t)))
			tc.prep(t, f)
			_, _, err := f.collect(t, nil)
			var se *core.SourceError
			if !errors.As(err, &se) || se.Reason != tc.reason {
				t.Fatalf("err %v, want %s", err, tc.reason)
			}
			if f.rt.count() != 0 {
				t.Fatal("request made without a key")
			}
		})
	}
}

// After a good read, a failure still serves the last reading (with its true observed time) until it is stale.
func TestLastGoodServedOnFailure(t *testing.T) {
	f := newFixt(t, "openclaw", "z", respond(200, globalBody(t)))
	_, blob, err := f.collect(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := f.now
	f.rt.handler = respond(500, nil)
	f.now = f.now.Add(10 * time.Minute)
	res, blob2, err := f.collect(t, blob)
	var se *core.SourceError
	if !errors.As(err, &se) || se.Reason != core.ReasonUpstream {
		t.Fatalf("%v", err)
	}
	if len(res.Samples) == 0 || !res.ObservedAt.Equal(first) || len(blob2) == 0 {
		t.Fatalf("last good not served: %+v", res)
	}
	f.now = first.Add(core.FreshnessLimit(f.src, f.src.StaleAfter) + time.Minute)
	res, _, _ = f.collect(t, blob2)
	if len(res.Samples) != 0 {
		t.Fatal("stale reading served")
	}
}

func TestUnknownLimitsCountedAsParse(t *testing.T) {
	f := newFixt(t, "openclaw", "z", respond(200, fixtureBytes(t, "quota-limit-unknown.json")))
	res, _, err := f.collect(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors[core.ReasonParse] != 4 || len(res.Samples) == 0 {
		t.Fatalf("%+v", res.Errors)
	}
}

func TestAuthBreaker(t *testing.T) {
	f := newFixt(t, "openclaw", "z", respond(401, nil))
	var blob json.RawMessage
	for i := 0; i < 2; i++ {
		_, b, err := f.collect(t, blob)
		if reasonErr(err) != core.ReasonAuthExpired {
			t.Fatalf("%v", err)
		}
		blob = b
	}
	if f.rt.count() != 2 {
		t.Fatalf("%d", f.rt.count())
	}
	_, blob, err := f.collect(t, blob)
	if reasonErr(err) != core.ReasonAuthExpired || f.rt.count() != 2 {
		t.Fatalf("third call must not reach the network: %d %v", f.rt.count(), err)
	}
	// the holder rewrites its file (new key revision): requests resume
	time.Sleep(5 * time.Millisecond)
	os.WriteFile(filepath.Join(f.src.Dir, "openclaw.json"), []byte(`{"models":{"providers":{"zai":{"apiKey":"`+keyB+`"}}}}`), 0o600)
	f.rt.handler = respond(200, globalBody(t))
	res, _, err := f.collect(t, blob)
	if err != nil || f.rt.count() != 3 || len(res.Samples) == 0 {
		t.Fatalf("breaker did not reset: %v %d", err, f.rt.count())
	}
	if f.rt.reqs[2].auth != keyB {
		t.Fatal("new key not used")
	}
}

func TestAuthBreakerOnEnvelopeCode(t *testing.T) {
	f := newFixt(t, "openclaw", "z", respond(200, []byte(`{"code":1000,"success":false}`)))
	var blob json.RawMessage
	for i := 0; i < 3; i++ {
		_, b, _ := f.collect(t, blob)
		blob = b
	}
	if f.rt.count() != 2 {
		t.Fatalf("%d requests", f.rt.count())
	}
}

func reasonErr(err error) string {
	var se *core.SourceError
	if errors.As(err, &se) {
		return se.Reason
	}
	return ""
}

func TestRestore(t *testing.T) {
	f := newFixt(t, "openclaw", "z", respond(200, globalBody(t)))
	_, blob, _ := f.collect(t, nil)
	f.now = f.now.Add(5 * time.Minute)
	res := f.src.Restore(blob)
	if len(res.Samples) == 0 || !res.ObservedAt.Equal(time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v", res)
	}
	f.now = f.now.Add(24 * time.Hour)
	if len(f.src.Restore(blob).Samples) != 0 {
		t.Fatal("stale restore served")
	}
	if len(f.src.Restore(nil).Samples) != 0 || len(f.src.Restore(json.RawMessage(`{`)).Samples) != 0 {
		t.Fatal("empty restore served")
	}
}

// The holder's files and directory are never written.
func TestHolderIsNeverWritten(t *testing.T) {
	f := newFixt(t, "openclaw", "z", respond(200, globalBody(t)))
	snap := func() string {
		var b strings.Builder
		filepath.Walk(f.src.Dir, func(p string, fi os.FileInfo, err error) error {
			if err == nil {
				fmt.Fprintf(&b, "%s %d %d %v|", p, fi.ModTime().UnixNano(), fi.Size(), fi.Mode())
			}
			return nil
		})
		return b.String()
	}
	before := snap()
	for i := 0; i < 3; i++ {
		f.collect(t, nil)
	}
	if snap() != before {
		t.Fatal("holder directory changed")
	}
}

// A profile that cannot read its key cannot tell whether a peer owns it: it serves nothing, and asks the
// scheduler to drop what an earlier poll cached.
func TestUnreadableKeyServesNothing(t *testing.T) {
	f := newFixt(t, "openclaw", "z", respond(200, globalBody(t)))
	_, blob, err := f.collect(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(f.src.Dir, "openclaw.json"))
	f.now = f.now.Add(10 * time.Minute)
	res, _, err := f.collect(t, blob)
	var se *core.SourceError
	if !errors.As(err, &se) || se.Reason != core.ReasonNotFound || !se.DropCache {
		t.Fatalf("err %v", err)
	}
	if len(res.Samples) != 0 {
		t.Fatal("persisted samples served without a readable key")
	}
	// a restart in the same state serves nothing either
	f.src.Now = func() time.Time { return f.now }
	if len(f.src.Restore(blob).Samples) != 0 {
		t.Fatal("restore served samples without a readable key")
	}
}
