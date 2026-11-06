package opencode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/events"
	"github.com/moophat/agent-quota-exporter/internal/sqlro"
)

type eventCursor struct {
	State   *dbState          `json:"state"`
	Next    *dbState          `json:"next,omitempty"`
	Pending []json.RawMessage `json:"pending,omitempty"`
}

func newGeneration() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func validCount(v any) bool { f, ok := num(v); return ok && f >= 0 && f <= 1<<53 && math.Trunc(f) == f }

// captureEvents processes at most one source page and one bounded pending set.
// A prepared envelope batch is durable before append, and progress is durable only
// after Append succeeds. Thus both write and checkpoint failures replay exact bytes.
func (s *DBSource) captureEvents(ctx context.Context, db *sqlro.DB, now time.Time, seed json.RawMessage, metricErr error, closeDB func() error) error {
	key := s.Host + "\x00" + s.Alias + "\x00" + Harness + "\x00" + SourceName
	raw, err := s.Events.Checkpoint(key)
	if err != nil {
		return err
	}
	c := eventCursor{}
	startup := false
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &c); err != nil {
			return err
		}
	} else {
		c.State = &dbState{}
		if err = json.Unmarshal(seed, c.State); err != nil {
			return err
		}
		// The initial boundary must survive an unavailable event checkpoint store.
		// Resolve first-seen startup now, rather than skipping later recovery rows.
		if !c.State.Init {
			startup = true
			last, e := db.MaxRowid(table)
			if e != nil {
				return e
			}
			c.State.Init = true
			c.State.Cursor = last
			c.State.Ino = fileIno(s.Path)
		}
		c.State.EventSeed = nil
		boundary, _ := json.Marshal(c.State)
		s.st.EventSeed = boundary
	}
	save := func() error {
		b, e := json.Marshal(c)
		if e != nil {
			return e
		}
		return s.Events.SaveCheckpoint(key, b)
	}
	if len(raw) == 0 {
		if c.State.Generation == "" {
			c.State.Generation = newGeneration()
		}
		if err = save(); err != nil {
			return err
		}
	}
	if len(c.Pending) > 0 {
		if err = s.Events.Append(events.Batch{Envelopes: c.Pending}); err != nil {
			return err
		}
		c.State = c.Next
		c.Next = nil
		c.Pending = nil
		if err = save(); err != nil {
			return err
		}
	}
	// Use the existing row/pending rules without affecting metric totals.
	metric := s.st
	stateBytes, err := json.Marshal(c.State)
	if err != nil {
		return err
	}
	nextState := &dbState{}
	if err = json.Unmarshal(stateBytes, nextState); err != nil {
		return err
	}
	s.st = nextState
	s.st.Totals = map[string]*total{}
	s.st.Named = map[string]int64{}
	if s.st.Pending == nil {
		s.st.Pending = map[string]*pend{}
	}
	tr := &collectTracker{bounded: true}
	tr.startupSkip = startup
	readErr := s.read(ctx, db, fileIno(s.Path), now, map[string]int{}, tr)
	closeErr := closeDB()
	next := s.st
	s.st = metric
	next.Totals = nil
	next.Named = nil
	var batch []json.RawMessage
	for _, m := range tr.completed {
		// Generation belongs to the event cursor, never the metric checkpoint.
		s.st = next
		b, e := s.buildUsageEvent(now, m)
		s.st = metric
		if e != nil {
			return e
		}
		batch = append(batch, b)
	}
	// A stale/missing cursor may revisit an already durable row. Recover its exact
	// original envelope rather than changing capture time or weakening conflict checks.
	if len(batch) > 0 {
		indexes := map[string]int{}
		for i, b := range batch {
			var e events.Envelope
			_ = json.Unmarshal(b, &e)
			indexes[e.EventID] = i
		}
		if err = s.Events.Replay(func(b json.RawMessage) error {
			var e events.Envelope
			if er := json.Unmarshal(b, &e); er != nil {
				return er
			}
			if i, ok := indexes[e.EventID]; ok {
				batch[i] = append(json.RawMessage(nil), b...)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	completion := "complete"
	if readErr != nil || metricErr != nil || closeErr != nil || ctx.Err() != nil {
		completion = "failed"
	} else if tr.backlog || tr.gap {
		completion = "partial"
	}
	f := collectFields{startedAt: now, endedAt: now, filesRead: 1, startupSkip: tr.startupSkip, pendingEvicted: tr.evicted, pendingAbandoned: tr.abandoned, inFlight: tr.inFlight, completion: completion}
	f.gap = tr.gap
	if readErr != nil || metricErr != nil || closeErr != nil {
		f.readFailed = true
	}
	b, err := s.buildCollectEvent(now, newGeneration(), f)
	if err != nil {
		return err
	}
	batch = append(batch, b)
	c.Next = next
	c.Pending = batch
	if err = save(); err != nil {
		return err
	}
	if err = s.Events.Append(events.Batch{Envelopes: batch}); err != nil {
		return err
	}
	c.State = next
	c.Next = nil
	c.Pending = nil
	return save()
}
