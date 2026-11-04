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
	localBackoff bool // backoffUntil applies to this instance only, not its gate group
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
	netBlock  map[string]time.Time // host-wide 429 backoff per gate group (NetGrouper)
	netLast   map[string]time.Time // last vendor call start per gate group
	wg        sync.WaitGroup
	saveError string
	// saveFailing blocks vendor calls while the state cannot be written: an attempt or a 429 backoff that is
	// not durable would be forgotten by a restart and the vendor asked again at once.
	saveFailing bool
}

// NewScheduler builds a scheduler. staleFloor is defaults.stale_after; the effective staleness per source is
// max(staleFloor, 3 × its interval) (contract, "Cardinality rules" 5).
func NewScheduler(srcs []Source, st *State, clock Clock, log *slog.Logger, staleFloor time.Duration) *Scheduler {
	s := &Scheduler{state: st, clock: clock, log: log, sem: make(chan struct{}, MaxConcurrentReads),
		netBlock: map[string]time.Time{}, netLast: map[string]time.Time{}}
	now := clock.Now()
	quarantined := false
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
			in.localBackoff = ps.LocalBackoff
		}
		if st.Quarantined && src.Network() {
			in.backoffUntil = now.Add(QuarantineBackoff)
			in.localBackoff = false
			quarantined = true
		}
		// Host-wide spacing survives a restart: the newest persisted vendor attempt counts as the last call.
		if g := netGroup(src); src.Network() && in.lastAttempt.After(s.netLast[g]) {
			s.netLast[g] = in.lastAttempt
		}
		if r, ok := src.(Restorer); ok && len(ps.Plugin) > 0 {
			if res := r.Restore(ps.Plugin); !res.ObservedAt.IsZero() && len(res.Samples) > 0 {
				in.cache, in.hasCache, in.observedAt = res.Samples, true, res.ObservedAt
			}
		}
		if g := netGroup(src); src.Network() && !in.localBackoff && in.backoffUntil.After(s.netBlock[g]) {
			s.netBlock[g] = in.backoffUntil
		}
		s.insts = append(s.insts, in)
		if quarantined {
			s.persistLocked(in) // the quarantine backoff must survive a restart within the hour
		}
	}
	if quarantined {
		s.save()
	}
	return s
}

// due reports whether in may run now. Caller holds s.mu.
func (s *Scheduler) due(in *instance, now time.Time, force bool) bool {
	if in.inflight || now.Before(in.backoffUntil) {
		return false
	}
	if in.src.Network() {
		g := netGroup(in.src)
		if s.saveFailing || now.Before(s.netBlock[g]) || (!s.netLast[g].IsZero() && now.Sub(s.netLast[g]) < NetSpacing) {
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
	type gate struct {
		lastAttempt time.Time
		attempted   bool
		netLast     time.Time
	}
	prev := map[*instance]gate{}
	s.mu.Lock()
	for _, in := range s.insts {
		if filter != nil && !filter(in.src) {
			continue
		}
		if !s.due(in, now, force) {
			continue
		}
		prev[in] = gate{in.lastAttempt, in.attempted, s.netLast[netGroup(in.src)]}
		in.inflight = true
		in.lastAttempt = now
		in.attempted = true
		s.persistLocked(in)
		if in.src.Network() {
			s.netLast[netGroup(in.src)] = now
		}
		started = append(started, in)
	}
	s.mu.Unlock()
	if len(started) == 0 {
		return
	}
	// Attempts are on disk before any vendor call, so a crash loop cannot re-send on every start. If they
	// cannot be written, vendor sources do not run.
	if err := s.save(); err != nil {
		kept := started[:0]
		s.mu.Lock()
		for _, in := range started {
			if in.src.Network() {
				// not run: undo the attempt so it is due again once the state is writable
				g := prev[in]
				in.inflight, in.lastAttempt, in.attempted = false, g.lastAttempt, g.attempted
				s.netLast[netGroup(in.src)] = g.netLast
				s.persistLocked(in)
				continue
			}
			kept = append(kept, in)
		}
		s.mu.Unlock()
		started = kept
	}
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
	timeout := ReadTimeout
	if t, ok := in.src.(Timeouter); ok && t.ReadTimeout() > 0 {
		timeout = t.ReadTimeout()
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
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
				in.localBackoff = reason != ReasonRateLimited
				if g := netGroup(in.src); in.src.Network() && reason == ReasonRateLimited && in.backoffUntil.After(s.netBlock[g]) {
					s.netBlock[g] = in.backoffUntil
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
	ps.LocalBackoff = in.localBackoff && !in.backoffUntil.IsZero()
	s.state.Put(in.key, ps)
}

func (s *Scheduler) save() error {
	err := s.state.Save()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.saveFailing = true
		if s.saveError != err.Error() {
			s.log.Error("cannot persist state; vendor sources paused until it can", "error", err.Error())
		}
		s.saveError = err.Error()
		return err
	}
	if s.saveFailing {
		s.log.Info("state persisted again; vendor sources resume")
	}
	s.saveFailing, s.saveError = false, ""
	return nil
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
		if g := netGroup(in.src); in.src.Network() && s.netBlock[g].After(sn.backoffUntil) {
			sn.backoffUntil = s.netBlock[g]
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
