package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
)

// HTTPSourceName is the contract `source` value of the live-session reader.
const HTTPSourceName = "opencode-http"

const (
	httpTimeout = 5 * time.Second
	httpMaxBody = 1 << 20
)

// HTTPSource is `opencode-http` for one profile: it asks a running opencode server which sessions are busy.
//
// It calls GET <url>/session/status only. The answer maps session ids to a status object and lists busy sessions
// alone ("{}" means none). Only the count of non-idle entries is kept: session ids and everything else in the
// answer are dropped on the spot. The server is local (the config check requires loopback unless allow_remote), the
// call needs no key, and nothing is written.
type HTTPSource struct {
	Host, Alias, Account string
	URL                  string
	Interval             time.Duration
	// Transport lets a test replace the network; nil uses a direct connection (no proxy from the environment).
	Transport http.RoundTripper
}

func (s *HTTPSource) Name() string               { return HTTPSourceName }
func (s *HTTPSource) Harness() string            { return Harness }
func (s *HTTPSource) Profile() string            { return s.Alias }
func (s *HTTPSource) MinInterval() time.Duration { return s.Interval }

// Network is true for a server beyond loopback (allow_remote): it then goes through the scheduler's network gates
// and is left out of the local-only one-shot run. A loopback server is local.
func (s *HTTPSource) Network() bool { return !config.LoopbackURL(s.URL) }

// NetGroup keeps the gate of one server apart from the vendor sources' shared gate.
func (s *HTTPSource) NetGroup() string { return "opencode-http/" + s.Alias }

// NetSpacing is the gap between two calls to the same server.
func (s *HTTPSource) NetSpacing() time.Duration { return 5 * time.Second }

// httpFail marks a read the source cannot vouch for: the count disappears and the source reports down.
func httpFail(reason string) error { return &core.SourceError{Reason: reason, DropCache: true} }

func (s *HTTPSource) Collect(ctx context.Context, _ time.Time, _ json.RawMessage) (core.Result, json.RawMessage, error) {
	res := core.Result{}
	rt := s.Transport
	if rt == nil {
		rt = &http.Transport{Proxy: nil, DisableKeepAlives: true}
	}
	client := &http.Client{Timeout: httpTimeout, Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.URL, "/")+"/session/status", nil)
	if err != nil {
		return res, nil, httpFail(core.ReasonOther)
	}
	resp, err := client.Do(req)
	if err != nil {
		var ne net.Error
		if ctx.Err() != nil || (errors.As(err, &ne) && ne.Timeout()) {
			return res, nil, httpFail(core.ReasonTimeout)
		}
		return res, nil, httpFail(core.ReasonUpstream)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return res, nil, httpFail(core.ReasonUpstream)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody+1))
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return res, nil, httpFail(core.ReasonTimeout)
		}
		return res, nil, httpFail(core.ReasonUpstream)
	}
	if len(body) > httpMaxBody {
		return res, nil, httpFail(core.ReasonParse)
	}
	var entries map[string]struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(body, &entries) != nil {
		return res, nil, httpFail(core.ReasonParse)
	}
	active := 0
	for _, e := range entries {
		// busy and retry are working; a type this build does not know is treated as working too. Only idle is not.
		if !strings.EqualFold(e.Type, "idle") {
			active++
		}
	}
	account := s.Account
	if account == "" {
		account = s.Alias
	}
	res.Samples = []core.Sample{{Name: "agent_sessions", Value: float64(active), Labels: []core.Label{
		{Name: "host", Value: s.Host}, {Name: "harness", Value: Harness}, {Name: "profile", Value: s.Alias},
		{Name: "account", Value: account}, {Name: "state", Value: "active"}}}}
	return res, nil, nil
}
