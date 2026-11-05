package claude

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// StatuslineCacheSource reads the AgentKit status line's vendor response without
// credentials or network access. App wiring binds it only to the default root profile.
type StatuslineCacheSource struct {
	P          *Profile
	Path       string
	Interval   time.Duration
	StaleAfter time.Duration

	mu         sync.Mutex
	readAt     time.Time
	quota      parsedQuota
	observedAt time.Time
	available  bool
}

func (s *StatuslineCacheSource) Name() string               { return "claude-statusline-cache" }
func (s *StatuslineCacheSource) Harness() string            { return Harness }
func (s *StatuslineCacheSource) Profile() string            { return s.P.Alias }
func (s *StatuslineCacheSource) MinInterval() time.Duration { return s.Interval }
func (s *StatuslineCacheSource) Network() bool              { return false }
func (s *StatuslineCacheSource) MaxAge() time.Duration      { return s.StaleAfter }

func (s *StatuslineCacheSource) reading(now time.Time) (parsedQuota, time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.readAt.IsZero() || now.Before(s.readAt) || now.Sub(s.readAt) >= s.Interval {
		s.read(now)
	}
	if !s.available || s.observedAt.After(now) || now.Sub(s.observedAt) > s.StaleAfter {
		return parsedQuota{}, time.Time{}, false
	}
	return s.quota, s.observedAt, true
}

func (s *StatuslineCacheSource) read(now time.Time) {
	s.readAt = now
	s.quota = parsedQuota{}
	s.observedAt = time.Time{}
	s.available = false

	b, err := readSmall(s.Path, maxBody)
	if err != nil || len(b) > maxBody {
		return
	}
	var doc struct {
		Timestamp int64           `json:"timestamp"`
		Status    string          `json:"status"`
		Data      json.RawMessage `json:"data"`
	}
	if json.Unmarshal(b, &doc) != nil || doc.Status != "available" || doc.Timestamp <= 0 {
		return
	}
	q, err := parseUsage(doc.Data)
	if err != nil {
		return
	}
	s.quota = q
	s.observedAt = time.UnixMilli(doc.Timestamp)
	s.available = true
}

func (s *StatuslineCacheSource) Collect(_ context.Context, now time.Time, _ json.RawMessage) (core.Result, json.RawMessage, error) {
	q, at, ok := s.reading(now)
	if !ok {
		return core.Result{}, nil, nil
	}
	_ = s.P.ResolveAccount()
	account, _ := s.P.Account()
	return core.Result{Samples: quotaSamples(s.P, account, q, at), ObservedAt: at}, nil, nil
}
