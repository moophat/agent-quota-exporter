package opencode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/schedtest"
)

const sessionCanary = "ses_CANARY-SESSION-ID"

func serve(t *testing.T, h http.HandlerFunc) (*HTTPSource, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &HTTPSource{Host: "node-a", Alias: "inst", Account: "inst", URL: srv.URL, Interval: 15 * time.Second}, srv
}

func reply(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session/status" || r.Method != http.MethodGet {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		w.Write([]byte(body))
	}
}

const activeLine = `agent_sessions{host="node-a",harness="opencode",profile="inst",account="inst",state="active"} `

func render(t *testing.T, s *HTTPSource) (*schedtest.H, string) {
	h := schedtest.New(t, time.Unix(1767225600, 0), s)
	h.Tick(time.Second)
	return h, h.Render(t)
}

func TestHTTPCountsBusyEntries(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"empty means none":   {`{}`, "0"},
		"two busy":           {`{"` + sessionCanary + `":{"type":"busy"},"ses_b":{"type":"busy"}}`, "2"},
		"idle is not active": {`{"ses_a":{"type":"idle"}}`, "0"},
		"retry is active":    {`{"ses_a":{"type":"retry","attempt":2,"message":"` + sessionCanary + `"}}`, "1"},
		"unknown is active":  {`{"ses_a":{"type":"compacting"},"ses_b":{"type":"idle"}}`, "1"},
		"no type is active":  {`{"ses_a":{}}`, "1"},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := serve(t, reply(tc.body))
			_, out := render(t, s)
			if !strings.Contains(out, activeLine+tc.want+"\n") {
				t.Fatalf("want %s%s in\n%s", activeLine, tc.want, out)
			}
			if strings.Contains(out, "CANARY") || strings.Contains(out, "ses_") {
				t.Fatal("session id or message in /metrics")
			}
			if !strings.Contains(out, `agent_source_up{host="node-a",harness="opencode",profile="inst",source="opencode-http"} 1`) {
				t.Fatal("source not up")
			}
		})
	}
}

// 500, a timeout, a bad body and an oversize body must remove the series and mark the source down after one failed
// read, not keep serving the last count.
func TestHTTPFailuresDropSeriesThroughScheduler(t *testing.T) {
	var mode atomic.Value
	mode.Store("ok")
	s, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load().(string) {
		case "500":
			http.Error(w, "boom", 500)
		case "redirect":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		case "oversize":
			w.Write([]byte(`{"x":{"type":"busy"},"pad":"` + strings.Repeat("a", httpMaxBody) + `"}`))
		case "garbage":
			w.Write([]byte(`[1,2,3]`))
		default:
			w.Write([]byte(`{"ses_a":{"type":"busy"}}`))
		}
	})
	h, out := render(t, s)
	if !strings.Contains(out, activeLine+"1\n") {
		t.Fatalf("first read:\n%s", out)
	}
	for _, m := range []string{"500", "redirect", "oversize", "garbage"} {
		mode.Store(m)
		h.Tick(time.Second)
		out = h.Render(t)
		if strings.Contains(out, "agent_sessions{") {
			t.Fatalf("%s: series survived a failed read:\n%s", m, out)
		}
		if !strings.Contains(out, `source="opencode-http"} 0`) {
			t.Fatalf("%s: source not down:\n%s", m, out)
		}
		mode.Store("ok")
		h.Tick(time.Second)
		if !strings.Contains(h.Render(t), activeLine+"1\n") {
			t.Fatalf("%s: no recovery", m)
		}
	}
}

func TestHTTPTimeoutIsClassified(t *testing.T) {
	var slow atomic.Bool
	release := make(chan struct{})
	s, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if slow.Load() {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		w.Write([]byte(`{"a":{"type":"busy"}}`))
	})
	defer close(release)
	slow.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _, err := s.Collect(ctx, time.Now(), nil)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("want timeout, got %v", err)
	}
}

func TestHTTPRedirectIsNotFollowed(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write([]byte(`{}`)) }))
	defer other.Close()
	s, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/session/status", http.StatusFound)
	})
	_, _, err := s.Collect(context.Background(), time.Now(), nil)
	if err == nil || hits.Load() != 0 {
		t.Fatalf("redirect followed (hits %d, err %v)", hits.Load(), err)
	}
}

func TestBuildAddsHTTPSourceOnlyWithURL(t *testing.T) {
	cfg := config.Default()
	with := config.Resolved{Harness: "opencode", Alias: "inst", Dir: t.TempDir(), URL: "http://127.0.0.1:4096"}
	without := config.Resolved{Harness: "opencode", Alias: "inst2", Dir: t.TempDir()}
	if n := len(Build(cfg, with, "node-a", nil)); n != 2 {
		t.Fatalf("with url: %d sources", n)
	}
	if n := len(Build(cfg, without, "node-a", nil)); n != 1 {
		t.Fatalf("without url: %d sources", n)
	}
}

// A server beyond loopback is a network source: it is not run by the local-only one-shot filter and has its own
// gate group, while a loopback one stays local.
func TestRemoteHTTPSourceIsANetworkSource(t *testing.T) {
	remote := &HTTPSource{Alias: "far", URL: "http://10.0.0.5:4096"}
	local := &HTTPSource{Alias: "near", URL: "http://127.0.0.1:4096"}
	if !remote.Network() || local.Network() {
		t.Fatalf("Network: remote %v local %v", remote.Network(), local.Network())
	}
	if remote.NetGroup() == "" || remote.NetGroup() == local.NetGroup() {
		t.Fatal("gate groups must be per server")
	}
}
