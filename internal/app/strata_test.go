package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
)

type laneClock struct{ at time.Time }

func (c *laneClock) Now() time.Time { return c.at }

func TestStrataWiringAndFailureStaleness(t *testing.T) {
	cfg := config.Default()
	b, err := Build(cfg, nil, "node-a", "test")
	if err != nil || len(b.Sources) != 0 {
		t.Fatalf("default sources: %+v %v", b.Sources, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slots" {
			w.Write([]byte(`[{"is_processing":true}]`))
		} else {
			w.Write([]byte(`{"live":{"queued":2},"totals":{"requests":9},"engine":{"model":"model-x","images":true,"max_context":1024}}`))
		}
	}))
	defer server.Close()
	cfg.Strata = []config.Lane{{Name: "lane-a", BaseURL: server.URL}}
	b, err = Build(cfg, nil, "node-a", "test")
	if err != nil || len(b.Sources) != 1 {
		t.Fatalf("sources: %+v %v", b.Sources, err)
	}
	clock := &laneClock{time.Unix(1800000000, 0)}
	st, err := core.OpenState(t.TempDir(), clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sched := core.NewScheduler(b.Sources, st, clock, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute)
	render := func() string {
		var out bytes.Buffer
		e := core.Exposition{Host: "node-a", Sched: sched, Clock: clock}
		if err := e.Render(&out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	sched.Tick(context.Background(), nil, false, true)
	if !strings.Contains(render(), `agent_lane_busy{host="node-a",lane="lane-a"} 1`) {
		t.Fatal(render())
	}
	server.Close()
	clock.at = clock.at.Add(40 * time.Second)
	sched.Tick(context.Background(), nil, false, true)
	output := render()
	if !strings.Contains(output, `reason="upstream"`) || !strings.Contains(output, `agent_lane_busy{`) {
		t.Fatal(output)
	}
	clock.at = clock.at.Add(time.Minute)
	output = render()
	if strings.Contains(output, "agent_lane_busy{") || !strings.Contains(output, `agent_source_up{host="node-a",harness="strata",profile="lane-a",source="strata"} 0`) {
		t.Fatal(output)
	}
}
