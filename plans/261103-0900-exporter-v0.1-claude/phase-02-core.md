---
phase: 2
title: Core — state, scheduler, renderer, HTTP, CLI
status: done
depends: [1]
---

# Phase 02 — core

## Files owned
`internal/core/{source.go,errors.go,state.go,scheduler.go,render.go,http.go}` + tests, `cmd/agent-quota-exporter/main.go`.

## Interfaces
```go
type Label struct{ Name, Value string }
type Sample struct{ Name string; Labels []Label; Value float64 }     // contract names only
type Source interface {
    Name() string                  // contract `source` vocabulary
    Harness() string               // claude-code
    Profile() string               // alias
    MinInterval() time.Duration
    Collect(ctx context.Context, now time.Time, st *PluginState) ([]Sample, error)
}
type SourceError struct{ Reason string; Backoff time.Duration; Msg string } // Reason ∈ contract set
```
`PluginState` is a `json.RawMessage` slot the plugin owns, persisted by the core.

## Steps
1. State store (design §3.3): `state_dir/state.json` {epoch, instances{key: {last_attempt, last_success,
   backoff_until, plugin}}}; key = harness/profile/source. Atomic write (tmp in same dir + fsync + rename).
   flock `state_dir/lock` (LOCK_EX|LOCK_NB) for process lifetime; held → exit 1 with clear message. state_dir
   created 0700 if missing. Corrupt state.json → log once, rename to `state.json.corrupt-<unix>`, start fresh.
2. Scheduler (design §3.1): tick 5 s; due = now ≥ last_attempt + min_interval·(1+jitter[0,0.1)) and now ≥
   backoff_until. Semaphore of 4; per-read ctx timeout 10 s; one in-flight per instance. On success: cache
   samples+observed_at, last_success. On SourceError: count `agent_source_errors_total{reason}`, set
   backoff_until if Backoff>0, log once per (instance, reason) state change. Non-SourceError → reason `other`.
   Panic in a plugin is recovered → reason `other`.
3. Renderer (design §3.6): families in contract order; HELP text from a table copied from the contract; series
   sorted; staleness drop (cached samples with observed_at < now − stale_after); static labels appended; ints
   printed as ints; label value escaping (`\\`, `\"`, `\n`). Health metrics per instance always:
   `agent_source_up` (1 if last attempt succeeded or cache fresh), `_last_success_timestamp_seconds` (absent if
   never), `_cache_age_seconds` (absent if no cache), `_backoff_until_timestamp_seconds` (0 when none),
   `_read_seconds`, `_errors_total`. Plus `agent_quota_exporter_build_info{version,contract="0"}`,
   `agent_quota_exporter_profiles{harness,origin}`, `agent_quota_exporter_state_epoch`,
   `process_start_time_seconds`.
4. HTTP: `GET /metrics` (text/plain; version=0.0.4), `GET /healthz` → `ok`; other paths 404; no other methods.
5. CLI: `--config`, `--once` (run every instance once synchronously, ignoring min_interval but honouring
   persisted backoff gates, print render to stdout, exit 0; exit 2 on config error), `--version`. Logging:
   `log/slog` text to stderr; never logs values from sources.

## Success criteria
- Scheduler test with fake clock: instance runs once per min_interval regardless of tick count; backoff honoured;
  plugin panic counted as `other`.
- State test: write/read roundtrip; second flock fails; corrupt file quarantined.
- Render test: escaping, ordering, staleness drop, static labels.
- `go test -race ./internal/core/...` green.
