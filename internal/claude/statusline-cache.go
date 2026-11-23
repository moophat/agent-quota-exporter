package claude

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// StatuslineCacheSource reads a usage cache file that a Claude Code status line
// script writes, without credentials or network access. The file holds the body
// of Claude's usage endpoint response wrapped as
// {"timestamp": <epoch ms>, "status": "available", "data": {...}}.
// App wiring creates it only for a profile that names the file explicitly.
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
func (s *StatuslineCacheSource) MaxAge() time.Duration      { return core.FreshnessLimit(s, s.StaleAfter) }

func (s *StatuslineCacheSource) reading(now time.Time) (parsedQuota, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.readAt.IsZero() || now.Before(s.readAt) || now.Sub(s.readAt) >= s.Interval {
		s.read(now)
	}
	if s.readErr != nil {
		return parsedQuota{}, time.Time{}, s.readErr
	}
	if s.observedAt.After(now) || now.Sub(s.observedAt) > s.MaxAge() {
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
	if !ok || !s.trustedOwner(st.Uid) || fi.Mode().Perm()&0022 != 0 {
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

// trustedOwner reports whether uid may own the cache file: root, the user the
// exporter runs as, or the owner of the profile directory (who can already
// change that profile's credentials). A profile directory that is a link is
// not trusted.
func (s *StatuslineCacheSource) trustedOwner(uid uint32) bool {
	if uid == 0 || int64(uid) == int64(os.Geteuid()) {
		return true
	}
	fi, err := os.Lstat(s.P.Dir)
	if err != nil || !fi.IsDir() {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uid
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
