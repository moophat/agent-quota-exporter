// Package schedtest drives sources through the real scheduler and renderer, so a test sees what /metrics would
// show after a read: the cache rules, source health and series removal included. Test use only.
package schedtest

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// Clock is a settable clock.
type Clock struct {
	mu sync.Mutex
	T  time.Time
}

func (c *Clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.T }
func (c *Clock) Add(d time.Duration) { c.mu.Lock(); c.T = c.T.Add(d); c.mu.Unlock() }

// H is a scheduler plus an exposition over the given sources.
type H struct {
	Clock *Clock
	Sched *core.Scheduler
	Exp   *core.Exposition
}

// New builds a harness with a temporary state dir. Time starts at start.
func New(t *testing.T, start time.Time, srcs ...core.Source) *H {
	t.Helper()
	clk := &Clock{T: start}
	st, err := core.OpenState(t.TempDir(), start)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sched := core.NewScheduler(srcs, st, clk, log, 45*time.Minute)
	exp := &core.Exposition{Host: "node-a", Version: "test", ProcessStart: start, Sched: sched, Clock: clk,
		Profiles: map[[2]string]int{}}
	return &H{Clock: clk, Sched: sched, Exp: exp}
}

// Tick advances time by step, then runs every source once, forced, and waits.
func (h *H) Tick(step time.Duration) {
	h.Clock.Add(step)
	h.Sched.Tick(context.Background(), nil, true, true)
}

// Render returns the /metrics text.
func (h *H) Render(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := h.Exp.Render(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
