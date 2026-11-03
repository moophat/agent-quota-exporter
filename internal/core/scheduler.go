package core

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Clock is injectable so tests (and golden output) are deterministic.
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// RealClock is the wall clock.
var RealClock Clock = realClock{}

const (
	// ReadTimeout bounds one Collect.
	ReadTimeout = 10 * time.Second
	// MaxConcurrentReads caps parallel reads across all sources.
	MaxConcurrentReads = 4
	// NetSpacing is the minimum gap between two vendor calls from this host (lock is per IP).
	NetSpacing = 30 * time.Second
	// QuarantineBackoff is applied to network sources when the state file had to be discarded.
	QuarantineBackoff = 60 * time.Minute
)

type instance struct {
	src        Source
	key        string
	jitter     float64
	staleAfter time.Duration

	// guarded by Scheduler.mu
	inflight     bool
	attempted    bool
	cache        []Sample
	observedAt   time.Time
	hasCache     bool
	lastAttempt  time.Time
	lastSuccess  time.Time
	backoffUntil time.Time
	lastOK       bool
	readSeconds  float64
	errors       map[string]float64
	loggedReason string
}

// Scheduler runs sources on their intervals and holds the cache /metrics renders from.
type Scheduler struct {
	mu        sync.Mutex
	insts     []*instance
	state     *State
	clock     Clock
	log       *slog.Logger
	sem       chan struct{}
	netBlock  time.Time // host-wide 429 backoff for network sources
	netLast   time.Time // last vendor call start
	wg        sync.WaitGroup
	saveError string
}

// NewScheduler builds a scheduler. staleFloor is defaults.stale_after; the effective staleness per source is
// max(staleFloor, 3 × its interval) (contract, "Cardinality rules" 5).
func NewScheduler(srcs []Source, st *State, clock Clock, log *slog.Logger, staleFloor time.Duration) *Scheduler {
	s := &Scheduler{state: st, clock: clock, log: log, sem: make(chan struct{}, MaxConcurrentReads)}
	now := clock.Now()
	for _, src := range srcs {
		key := src.Harness() + "/" + src.Profile() + "/" + src.Name()
		h := fnv.New32a()
		h.Write([]byte(key))
		stale := 3 * src.MinInterval()
		if staleFloor > stale {
			stale = staleFloor
		}
		in := &instance{src: src, key: key, jitter: float64(h.Sum32()%1000) / 10000, staleAfter: stale,
			errors: map[string]float64{}}
		ps := st.Get(key)
		if ps.LastAttempt > 0 {
			in.lastAttempt = time.Unix(ps.LastAttempt, 0)
			in.attempted = true
		}
		if ps.LastSuccess > 0 {
			in.lastSuccess = time.Unix(ps.LastSuccess, 0)
		}
		if ps.BackoffUntil > 0 {
			in.backoffUntil = time.Unix(ps.BackoffUntil, 0)
		}
		if st.Quarantined && src.Network() {
			in.backoffUntil = now.Add(QuarantineBackoff)
		}
		if src.Network() && in.backoffUntil.After(s.netBlock) {
			s.netBlock = in.backoffUntil
		}
		s.insts = append(s.insts, in)
	}
	return s
}

// due reports whether in may run now. Caller holds s.mu.
func (s *Scheduler) due(in *instance, now time.Time, force bool) bool {
	if in.inflight || now.Before(in.backoffUntil) {
		return false
	}
	if in.src.Network() {
		if now.Before(s.netBlock) || (!s.netLast.IsZero() && now.Sub(s.netLast) < NetSpacing) {
			return false
		}
		// Network sources never bypass their interval, not even when forced (--once).
		force = false
	}
	if force || !in.attempted {
		return true
	}
	next := in.lastAttempt.Add(time.Duration(float64(in.src.MinInterval()) * (1 + in.jitter)))
	return !now.Before(next)
}

// Tick starts every due instance accepted by filter (nil = all). force runs local sources regardless of their
// interval. wait blocks until the started reads finish.
func (s *Scheduler) Tick(ctx context.Context, filter func(Source) bool, force, wait bool) {
	now := s.clock.Now()
	var started []*instance
	s.mu.Lock()
	for _, in := range s.insts {
		if filter != nil && !filter(in.src) {
			continue
		}
		if !s.due(in, now, force) {
			continue
		}
		in.inflight = true
		in.lastAttempt = now
		in.attempted = true
		s.persistLocked(in)
		if in.src.Network() {
			s.netLast = now
		}
		started = append(started, in)
	}
	s.mu.Unlock()
	if len(started) == 0 {
		return
	}
	// Attempts are on disk before any vendor call, so a crash loop cannot re-send on every start.
	s.save()
	for _, in := range started {
		s.wg.Add(1)
		go func(in *instance) {
			defer s.wg.Done()
			s.sem <- struct{}{}
			defer func() { <-s.sem }()
			s.run(ctx, in, now)
		}(in)
	}
	if wait {
		s.wg.Wait()
	}
}

// Wait blocks until every started read has finished.
func (s *Scheduler) Wait() { s.wg.Wait() }

// Loop ticks every 5 s until ctx is done.
func (s *Scheduler) Loop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	s.Tick(ctx, nil, false, false)
	for {
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		case <-t.C:
			s.Tick(ctx, nil, false, false)
		}
	}
}

func (s *Scheduler) run(ctx context.Context, in *instance, start time.Time) {
	rctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	ps := s.state.Get(in.key)

	var res Result
	var newState json.RawMessage
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = Fail(ReasonOther)
				s.log.Error("plugin panic recovered", "source", in.src.Name(), "profile", in.src.Profile())
			}
		}()
		res, newState, err = in.src.Collect(rctx, start, ps.Plugin)
	}()
	if err == nil && rctx.Err() != nil && errors.Is(rctx.Err(), context.DeadlineExceeded) && len(res.Samples) == 0 {
		err = Fail(ReasonTimeout)
	}
	end := s.clock.Now()

	s.mu.Lock()
	in.inflight = false
	in.readSeconds = end.Sub(start).Seconds()
	if newState != nil {
		ps.Plugin = newState
	}
	for r, n := range res.Errors {
		in.errors[r] += float64(n)
	}
	if err == nil {
		in.lastOK = true
		in.lastSuccess = start
		in.cache = res.Samples
		in.hasCache = true
		in.observedAt = start
		if !res.ObservedAt.IsZero() {
			in.observedAt = res.ObservedAt
		}
		if in.loggedReason != "" {
			s.log.Info("source recovered", "source", in.src.Name(), "profile", in.src.Profile())
			in.loggedReason = ""
		}
	} else {
		in.lastOK = false
		var se *SourceError
		reason := ReasonOther
		if errors.As(err, &se) {
			reason = se.Reason
			if se.Backoff > 0 {
				in.backoffUntil = start.Add(se.Backoff)
				if in.src.Network() && reason == ReasonRateLimited && in.backoffUntil.After(s.netBlock) {
					s.netBlock = in.backoffUntil
				}
			}
		}
		// A plugin may serve persisted values (e.g. last good quota) alongside an error.
		if len(res.Samples) > 0 && !res.ObservedAt.IsZero() {
			in.cache, in.hasCache, in.observedAt = res.Samples, true, res.ObservedAt
		}
		in.errors[reason]++
		if in.loggedReason != reason {
			s.log.Warn("source failed", "source", in.src.Name(), "profile", in.src.Profile(), "reason", reason)
			in.loggedReason = reason
		}
	}
	s.state.Put(in.key, ps)
	s.persistLocked(in)
	s.mu.Unlock()
	s.save()
}

// persistLocked copies gate fields into the state store. Caller holds s.mu.
func (s *Scheduler) persistLocked(in *instance) {
	ps := s.state.Get(in.key)
	ps.LastAttempt = unixOrZero(in.lastAttempt)
	ps.LastSuccess = unixOrZero(in.lastSuccess)
	ps.BackoffUntil = unixOrZero(in.backoffUntil)
	s.state.Put(in.key, ps)
}

func (s *Scheduler) save() {
	if err := s.state.Save(); err != nil {
		s.mu.Lock()
		changed := s.saveError != err.Error()
		s.saveError = err.Error()
		s.mu.Unlock()
		if changed {
			s.log.Error("cannot persist state", "error", err.Error())
		}
	}
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// snapshot is a consistent copy of everything render needs.
type snapshot struct {
	key, name, harness, profile string
	network                     bool
	attempted, lastOK, hasCache bool
	fresh                       bool
	cache                       []Sample
	observedAt, lastSuccess     time.Time
	backoffUntil                time.Time
	readSeconds                 float64
	errors                      map[string]float64
}

func (s *Scheduler) snapshot(now time.Time) []snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]snapshot, 0, len(s.insts))
	for _, in := range s.insts {
		sn := snapshot{key: in.key, name: in.src.Name(), harness: in.src.Harness(), profile: in.src.Profile(),
			network: in.src.Network(), attempted: in.attempted, lastOK: in.lastOK, hasCache: in.hasCache,
			observedAt: in.observedAt, lastSuccess: in.lastSuccess, backoffUntil: in.backoffUntil,
			readSeconds: in.readSeconds, errors: map[string]float64{}}
		if in.src.Network() && s.netBlock.After(sn.backoffUntil) {
			sn.backoffUntil = s.netBlock
		}
		sn.fresh = in.hasCache && now.Sub(in.observedAt) <= in.staleAfter
		if sn.fresh {
			sn.cache = append([]Sample(nil), in.cache...)
		}
		for k, v := range in.errors {
			sn.errors[k] = v
		}
		out = append(out, sn)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}
