package claude

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"sync"
	"syscall"
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
	readErr    error
}

func (s *StatuslineCacheSource) Name() string               { return "claude-statusline-cache" }
func (s *StatuslineCacheSource) Harness() string            { return Harness }
func (s *StatuslineCacheSource) Profile() string            { return s.P.Alias }
func (s *StatuslineCacheSource) MinInterval() time.Duration { return s.Interval }
func (s *StatuslineCacheSource) Network() bool              { return false }
func (s *StatuslineCacheSource) MaxAge() time.Duration      { return s.StaleAfter }

func (s *StatuslineCacheSource) reading(now time.Time) (parsedQuota, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.readAt.IsZero() || now.Before(s.readAt) || now.Sub(s.readAt) >= s.Interval {
		s.read(now)
	}
	if s.readErr != nil {
		return parsedQuota{}, time.Time{}, s.readErr
	}
	if s.observedAt.After(now) || now.Sub(s.observedAt) > s.StaleAfter {
		return parsedQuota{}, time.Time{}, core.Fail(core.ReasonOther)
	}
	// OpenRegular stats the safely opened descriptor; never read credentials.
	f, fi, err := openRegular(filepath.Join(s.P.Dir, ".credentials.json"))
	if err != nil {
		return parsedQuota{}, time.Time{}, err
	}
	f.Close()
	if s.observedAt.Before(fi.ModTime()) {
		return parsedQuota{}, time.Time{}, core.Fail(core.ReasonAuthExpired)
	}
	return s.quota, s.observedAt, nil
}

func (s *StatuslineCacheSource) read(now time.Time) {
	s.readAt = now
	s.quota = parsedQuota{}
	s.observedAt = time.Time{}
	s.readErr = core.Fail(core.ReasonParse)

	f, fi, err := openRegular(s.Path)
	if err != nil {
		s.readErr = err
		return
	}
	defer f.Close()
	// fi comes from fstat on this same descriptor, before any bytes are read.
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || fi.Mode().Perm()&0022 != 0 {
		s.readErr = core.Fail("untrusted_owner")
		return
	}
	if fi.Size() > maxBody {
		return
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBody+1))
	if err != nil {
		s.readErr = core.Fail(core.ReasonOther)
		return
	}
	if len(b) > maxBody {
		return
	}
	var doc struct {
		Timestamp int64           `json:"timestamp"`
		Status    string          `json:"status"`
		Data      json.RawMessage `json:"data"`
	}
	if json.Unmarshal(b, &doc) != nil || doc.Timestamp <= 0 {
		return
	}
	if doc.Status != "available" {
		s.readErr = core.Fail(core.ReasonUpstream)
		return
	}
	q, err := parseUsage(doc.Data)
	if err != nil {
		return
	}
	s.quota = q
	s.observedAt = time.UnixMilli(doc.Timestamp)
	s.readErr = nil
}

func (s *StatuslineCacheSource) Collect(_ context.Context, now time.Time, _ json.RawMessage) (core.Result, json.RawMessage, error) {
	q, at, err := s.reading(now)
	if err != nil {
		return core.Result{}, nil, err
	}
	_ = s.P.ResolveAccount()
	account, _ := s.P.Account()
	return core.Result{Samples: quotaSamples(s.P, account, q, at), ObservedAt: at}, nil, nil
}
