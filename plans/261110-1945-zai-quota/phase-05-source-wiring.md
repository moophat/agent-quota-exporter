# Phase 5: `zai-usage` source and wiring

## Context
Pattern: `codex.UsageSource` (`Collect`, `Restore`, `get`, 401 breaker, `rateLimited`, `NetGroup`, `NetSpacing`) and `codex.Build`. Read
`internal/app/app.go`, `internal/app/e2e_test.go`, `internal/core/scheduler.go` (gates, persistence of state blob).

## Files (owned)
New: `internal/zai/source.go`, `build.go`, `peers.go`, `source_test.go`, `peers_test.go`, `redact_test.go`.
Modify: `internal/app/app.go` (add `case config.HarnessZAI`), `internal/app/e2e_test.go` or new `internal/app/zai_test.go`.

## Behaviour (see plan.md for the tables)
- `Source`: `Name()=="zai-usage"`, `Harness()=="zai"`, `Network()==true`, `NetGroup()=="zai"`, `NetSpacing()==5s`, `MinInterval` = config interval
  (default 10 min). Fields: Host, Alias, Account, Dir, KeySource, Region, Version, Interval, Backoff429, StaleAfter, `Peers *Peers`; test-only
  `BaseURL`, `Transport`, `Now` (not reachable from config, as in codex).
- `Collect`: `peers.owner(self)` (non-owner: return empty `Result`, no request, keep state); read key via the reader; choose host from region
  (config > key hint > global); one `GET <host>/api/monitor/usage/quota/limit`, header `Authorization: <key>` (no `Bearer`), `Accept`,
  `User-Agent: agent-quota-exporter/<version>`; no proxy, no keep-alive, no redirects, 10 s timeout, 1 MiB body cap; `req.Header.Del` after `Do`;
  `Zero()` the key right after. Errors per the plan's failure table. Success -> persist `reading` + `LastAt` + `fp`; serve via phase 4.
- State blob: readings, `LastAt`, `C401`, `FP` (file fingerprint, never a key hash). Two auth failures with equal `FP` -> skip calls until `FP` changes.
- `Restore` serves persisted readings with their true observed time.
- `peers.go`: `Peers{sources []*Source}`; `owner` hashes each readable peer key in memory (sha256), owner = smallest alias among equal hashes;
  `sync.Once`-style single `slog` warning naming aliases only.
- `Build(cfg, r, host, version, peers)`: one source per resolved zai profile.

## Tests first (fake `http.RoundTripper`, temp dirs with phase 2-3 fixtures; no network)
1. Happy path via each key source: exact request (method, host, path, `Authorization` equals the raw key, no `Bearer`, no body); metrics from phase 4;
   `agent_source_up 1`; observed time = now.
2. Region: config beats hint beats default; `cn` goes to `open.bigmodel.cn`; a test pins that no other host is ever requested.
3. Each failure row of the plan table maps to its reason, including 200+`success:false` codes; 429 sets `SourceError.Backoff` (default 30 min, override honoured);
   last good readings still served (until freshness limit) with the error.
4. Breaker: two 401s same `FP` -> no third request; rewriting the key file (new `fp`) -> request resumes.
5. Duplicate key: openclaw + opencode profiles with the same key -> exactly one request per round, the smaller alias owns it, the other emits nothing and is `up`;
   different keys -> two requests; owner key rotates -> ownership recomputed; unreadable peer ignored; single warning.
6. Canary: run a full collect with failing and succeeding transports; capture `slog` output, error strings, rendered `/metrics`, state blob, and any fixture-side
   output; none contains the key bytes or its base64/hex/sha256-prefix.
7. Restore after restart serves the last reading; stale reading not served.
8. e2e (`internal/app`): config with two zai profiles through `Load -> Resolve -> Build -> render` shows the series and `agent_quota_exporter_profiles{harness="zai"}`;
   `--once` without `--once-with-quota` makes no request (`Network()==true`).
9. Read-only: file mtimes/inodes of the holder dir unchanged after collects (the holder is never written).

## Validation
`go test ./...`, `go vet ./...`, `gofmt -l .` empty.

## Risk and rollback
Highest-risk phase (credentials + network). Mitigated by tests 2, 6, 9. Rollback: remove the `case` in `app.Build`; the source is then never built.
