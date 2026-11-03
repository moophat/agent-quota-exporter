// Package core owns timing, caching, gates, persistence and exposition. Plugins only read and return facts
// (specs/plan.md, "Architecture").
package core

import (
	"context"
	"encoding/json"
	"time"
)

// Label is one Prometheus label pair.
type Label struct{ Name, Value string }

// Sample is one series value. Name must be a family listed in families (render.go).
type Sample struct {
	Name   string
	Labels []Label
	Value  float64
}

// Result is what a plugin returns from one Collect.
type Result struct {
	Samples []Sample
	// ObservedAt is when the samples were true; zero means "now". A plugin serving values persisted before a
	// restart sets the original time so staleness still applies.
	ObservedAt time.Time
	// Errors are non-fatal failures within an otherwise successful read, by contract reason (e.g. parse).
	Errors map[string]int
}

// Source is one plugin bound to one profile.
type Source interface {
	Name() string    // contract `source` vocabulary, e.g. claude-transcripts
	Harness() string // e.g. claude-code
	Profile() string // profile alias
	MinInterval() time.Duration
	// Network is true for sources that call a vendor: their attempts are persisted before the call and they
	// share host-wide spacing and 429 backoff (a vendor lock is per IP).
	Network() bool
	// Collect reads the source. state is the plugin's persisted blob (nil on first run). A non-nil returned
	// state replaces it, also when err != nil (e.g. a 401 counter). A plugin must only record progress for work
	// it finished.
	Collect(ctx context.Context, now time.Time, state json.RawMessage) (Result, json.RawMessage, error)
}

// Contract `reason` values for agent_source_errors_total.
const (
	ReasonNotFound    = "not_found"
	ReasonPermission  = "permission"
	ReasonParse       = "parse"
	ReasonTimeout     = "timeout"
	ReasonAuthExpired = "auth_expired"
	ReasonRateLimited = "rate_limited"
	ReasonUpstream    = "upstream"
	ReasonOther       = "other"
)

// SourceError is a classified failure. It deliberately carries no message: free text from the OS or a vendor
// can hold paths, bodies or tokens, so only the bounded reason ever leaves the plugin.
type SourceError struct {
	Reason  string
	Backoff time.Duration
}

func (e *SourceError) Error() string { return "source error: " + e.Reason }

// Fail returns a SourceError with the given reason.
func Fail(reason string) error { return &SourceError{Reason: reason} }
