package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// ProcAlive reports whether a pid exists on this host. Replaceable in tests.
var ProcAlive = func(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

// SessionsSource is `claude-sessions`: live session count by state from <dir>/sessions/<pid>.json.
type SessionsSource struct {
	P        *Profile
	Interval time.Duration
}

func (s *SessionsSource) Name() string               { return "claude-sessions" }
func (s *SessionsSource) Harness() string            { return Harness }
func (s *SessionsSource) Profile() string            { return s.P.Alias }
func (s *SessionsSource) MinInterval() time.Duration { return s.Interval }
func (s *SessionsSource) Network() bool              { return false }

// sessionState maps the harness status word to the contract's bounded state. Only "idle" is verified on a
// real install (fixtures/claude/sessions); the busy words are best effort and fall back to "other".
func sessionState(status string) string {
	switch strings.ToLower(status) {
	case "idle":
		return "idle"
	case "busy", "running", "active", "working", "thinking":
		return "active"
	default:
		return "other"
	}
}

func (s *SessionsSource) Collect(_ context.Context, _ time.Time, _ json.RawMessage) (core.Result, json.RawMessage, error) {
	counts := map[string]float64{"active": 0, "idle": 0, "other": 0}
	res := core.Result{}
	ents, err := os.ReadDir(filepath.Join(s.P.Dir, "sessions"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, nil, core.Fail(core.ErrReason(err))
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := readSmall(filepath.Join(s.P.Dir, "sessions", e.Name()), 1<<20)
		if err != nil {
			continue // a session file can vanish between list and read
		}
		var doc struct {
			PID    int    `json:"pid"`
			Status string `json:"status"`
		}
		if json.Unmarshal(b, &doc) != nil || doc.PID <= 0 {
			if res.Errors == nil {
				res.Errors = map[string]int{}
			}
			res.Errors[core.ReasonParse]++
			continue
		}
		if !ProcAlive(doc.PID) {
			continue
		}
		counts[sessionState(doc.Status)]++
	}
	label, _ := s.P.Account()
	for _, st := range []string{"active", "idle", "other"} {
		res.Samples = append(res.Samples, core.Sample{Name: "agent_sessions",
			Labels: s.P.labels(label, core.Label{Name: "state", Value: st}), Value: counts[st]})
	}
	return res, nil, nil
}
