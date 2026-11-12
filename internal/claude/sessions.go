package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// SessionsSource is `claude-sessions`: live session count by state from <dir>/sessions/<pid>.json.
//
// With PerSession set it also exports one series group per live session (session-detail.go).
type SessionsSource struct {
	P           *Profile
	Interval    time.Duration
	PerSession  bool
	MaxSessions int // per-session cap; 0 = defaultMaxSessions
}

func (s *SessionsSource) Name() string               { return "claude-sessions" }
func (s *SessionsSource) Harness() string            { return Harness }
func (s *SessionsSource) Profile() string            { return s.P.Alias }
func (s *SessionsSource) MinInterval() time.Duration { return s.Interval }
func (s *SessionsSource) Network() bool              { return false }

// sessionState maps the harness status word to the contract's bounded state. The file is undocumented upstream.
// Seen on real installs (2026-11): "busy" and "idle", plus "shell", whose meaning is unknown and which stays
// "other" until it is pinned down. A third-party report adds "waiting" (a permission or selection dialog), also
// "other". The remaining busy words are best effort.
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
	if err := realDir(s.P.Dir); err != nil {
		return res, nil, dropCache(err)
	}
	dir := filepath.Join(s.P.Dir, "sessions")
	var ents []os.DirEntry
	if df, err := openDir(dir); err == nil {
		ents, err = df.ReadDir(-1)
		df.Close()
		if err != nil {
			return res, nil, dropCache(core.Fail(core.ErrReason(err)))
		}
	} else {
		var se *core.SourceError
		if !errors.As(err, &se) || se.Reason != core.ReasonNotFound {
			return res, nil, dropCache(err) // present but a link or not a directory
		}
	}
	var live []sessionDoc
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := readSmall(filepath.Join(dir, e.Name()), 1<<20)
		if err != nil {
			continue // a session file can vanish between list and read
		}
		var doc sessionDoc
		if json.Unmarshal(b, &doc) != nil || doc.PID <= 0 {
			if res.Errors == nil {
				res.Errors = map[string]int{}
			}
			res.Errors[core.ReasonParse]++
			continue
		}
		if !sessionAlive(doc) {
			continue
		}
		counts[sessionState(string(doc.Status))]++
		live = append(live, doc)
	}
	label, _ := s.P.Account()
	for _, st := range []string{"active", "idle", "other"} {
		res.Samples = append(res.Samples, core.Sample{Name: "agent_sessions",
			Labels: s.P.labels(label, core.Label{Name: "state", Value: st}), Value: counts[st]})
	}
	if s.PerSession {
		var over int
		var per []core.Sample
		per, over = sessionSamples(s.P, label, live, s.MaxSessions)
		res.Samples = append(res.Samples, per...)
		if over > 0 {
			if res.Errors == nil {
				res.Errors = map[string]int{}
			}
			res.Errors[core.ReasonOther] += over
		}
	}
	return res, nil, nil
}

// dropCache marks a failed read so the scheduler discards the last good sessions: a session list that cannot be
// read is unknown, and must not keep showing sessions that may have ended.
func dropCache(err error) error {
	var se *core.SourceError
	if errors.As(err, &se) {
		c := *se
		c.DropCache = true
		return &c
	}
	return &core.SourceError{Reason: core.ReasonOther, DropCache: true}
}
