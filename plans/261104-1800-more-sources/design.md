# v0.2 design: shared decisions before the plugins

Companion to `plan.md`. Written by the cloud lane (claude-opus-5-5), 2026-11-04, before any v0.2 code.

## 1. Passive quota staleness

The plan's decision: a passive quota series stays until its own `resets_at` passes. `agent_quota_observed_timestamp_seconds`
carries its true age.

Mechanism, chosen so the core needs no change:

- A passive plugin re-reads local state every interval. A successful read returns `ObservedAt` = now, so the core's
  read-based staleness (`max(stale_after, 3 × interval)`) only drops values when the *read* keeps failing. That is right:
  if the files vanish, the values must go.
- The plugin itself omits every window whose `resets_at` is in the past at read time. A window with no `resets_at` falls
  back to `stale_after` measured from its event time.
- The plugin emits `agent_quota_observed_timestamp_seconds` = the event's own time, not the read time. The consumer shows
  "as of".
- Contract text changes to name this exception: passive sources only, per source. Series and types are unchanged.

## 2. New metrics (additive, contract stays 0)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `agent_quota_credits_available` | gauge 0/1 | `host harness profile account provider scope` | 1 if the vendor reports credits left or unlimited, 0 if out |
| `agent_quota_credits_balance` | gauge | same | balance as reported, in the vendor's own credit unit; absent if unlimited or not reported |
| `agent_router_connection_up` | gauge 0/1 | `host harness profile account provider` | 1 if the connection is active, its last test did not fail and it is not rate-limited now |
| `agent_router_rate_limited_until_timestamp_seconds` | gauge | same | the router's own rate-limit end; absent when none |
| `agent_provider_balance` | gauge | `host provider account unit` | balance as returned by the configured endpoint; `unit` from config (fixed per account) |

The router reuses `account` for the connection: its configured alias, else `acct-` + sha256(connection id)[:8]. An
email is never the label unless `account_label: email`, as for Claude.

Counters reuse `agent_tokens_total`, `agent_requests_total`, `agent_request_errors_total` and `agent_cost_usd_total`
with their existing label sets.

## 3. Label vocabulary for the new sources

- `window`: from minutes, `300` → `5h`, `10080` → `7d`. agy `5h` → `5h`, `weekly` → `7d`. Anything else → `other`.
- `scope`: Codex `limit_id` passes through only if it matches `^[a-z0-9_-]{1,32}$`, else `other`. agy group from the
  bucket id prefix, `gemini` or `3p`, else `other`.
- `type` for tokens: `input` is uncached input. Codex `input_tokens` includes `cached_input_tokens` (the fixture's
  `total_tokens` = `input_tokens` + `output_tokens`), so `input` = `input_tokens − cached_input_tokens −
  cache_write_input_tokens`, floored at 0. `cache_read` = cached, `cache_write` = cache write, `reasoning` =
  `reasoning_output_tokens`, `output` = `output_tokens − reasoning_output_tokens` only if Codex counts reasoning inside
  output. The codex builder confirms that from the fixture and documents the choice in `specs/sources.md`.
- `account` for Codex and opencode: the profile's configured `account`, else the profile alias. Nothing is read from
  `auth.json` or the account table.

## 4. Read-only SQLite (`internal/sqlro`)

- Driver `modernc.org/sqlite` (pure Go, as `specs/plan.md` chose), so the release stays `CGO_ENABLED=0`.
- `Open(path)`: checks every path component with `Lstat` (no symlinks, as for Claude). Opens
  `file:<path>?mode=ro` with `query_only` and a short busy timeout, never `immutable`, since the DB is live with WAL.
- No raw SQL from plugins. A query is built from an allow-list `table → columns`, where a column can be
  `json_extract(<col>, '$.<path>')` only if that `(table, col, path)` triple is listed. The `WHERE` is fixed, the form
  `<id col> > ?`, plus a `LIMIT`. Anything outside the list returns an error before SQL is built.
- A test per table: selecting a secret column (`threads.first_user_message`, `providerConnections.data` whole,
  `usageHistory.apiKey`, any column of `account` or `credential`) fails; the listed extracts succeed.
- SQLite busy: return `ReasonTimeout` and retry next cycle; never wait on a lock held by a live agent.

## 5. Config and discovery

- Harnesses: `claude-code` (v0.1), `codex`, `opencode`, `9router`, `agy`, plus the `provider-balance` section below.
- `profiles[].dir` meaning per harness: `codex` = a `CODEX_HOME`; `opencode` = the data dir holding `opencode.db`;
  `9router` = the `.9router` dir; `agy` = the `HOME` to run `agy` under.
- Discovery (`mode: add`/`only`) adds `<root>/.codex` (has `sessions/`) and `<root>/.local/share/opencode` (has
  `opencode.db`). Nothing else is discovered: router and agy profiles exist only when declared, which is the operator's
  choice to enable them. The same goes for reading one agy account from one host.
- Opt-in keys in `quota:` gain `agy_usage`. `codex_rate_limits` stays reserved, not built.
- `provider_balances:` top-level list: `provider`, `account`, `url`, `json_path`, `unit`, `key` (`env:NAME` or
  `file:/path`), `header` (default `Authorization: Bearer`), `interval` (default 15m, floor 5m).

## 6. Wiring and file ownership

The scaffold commit adds every package with a stub `Build` and the `app.Build` switch, so plugin work never edits
shared files except its own `specs/sources.md` section and fixtures.

| Work | Owns |
|---|---|
| Scaffold (cloud lead) | `internal/config/*`, `internal/core/render.go` families, `internal/sqlro/*`, `internal/app/app.go`, `specs/metric-contract.md`, `go.mod` |
| codex-rollouts + credits | `internal/codex/*`, `fixtures/codex/*`, `specs/sources.md` Codex section |
| opencode-db | `internal/opencode/*`, `fixtures/opencode/*`, `specs/sources.md` opencode section |
| router-9router | `internal/router/*`, `fixtures/router/*`, `specs/sources.md` router section |
| agy-usage | `internal/agy/*`, `fixtures/agy/*`, `specs/sources.md` agy section |
| provider balances | `internal/balance/*`, `specs/sources.md` balances section |

## 7. What stays out

- Codex session counts. The plan asks for them, but Codex writes no live-session file with a pid, so a count would be a
  guess from file mtimes. Left out until a real signal exists.
- `codex-rate-limits` (active): not built, per plan item 7.
- `opencode-http`: not built; the DB carries everything the page needs.

## 8. Red-team verdicts (2026-11-04, before code)

A hostile review raised 13 findings. Verdicts, which override the sections above where they differ:

1. **Credits vanished after 30 min** (blocker, accepted). Credits stay until a newer event replaces them, capped at the
   longest window's `resets_at` from the same event, or event + 7 d when none. A fixture fact also changes the meaning:
   a normal plan reports `has_credits: false` with low windows (`fixtures/codex/rollouts`). So `credits_available = 0` is
   *not* "out of credits" by itself. The contract says so and tells consumers to alert on a fall from 1 to 0. The
   on-prem lane is asked for a rollout written during a real credits outage. Codex `error` events go to
   `agent_request_errors_total{code}` only once a real sample shows their shape.
2. **No quota until the next turn after a fresh start** (high, accepted). Quota is a gauge, not a counter, so on start the
   plugin scans the newest rollouts (by mtime, whole tree) for the last `rate_limits`. It persists that, and a null
   `rate_limits` never overwrites it.
3. **Total-delta double counts** (high, accepted). Count `last_token_usage`. `total_token_usage` serves only to skip
   duplicates (unchanged total). Skip events older than the file's first sighting minus a grace period, as v0.1 does.
   Persist the current `turn_context` model per file.
4. **Token arithmetic unproven** (medium, accepted). It is settled from the Codex source before coding, and a fixture
   with non-zero cache write and reasoning is added.
5. **opencode `id > ?` misses updated rows** (blocker, accepted). Use a `rowid` cursor plus a persisted set of incomplete
   assistant ids, re-read by primary key until they complete. `sqlro` gets exactly two fixed query forms: rows after a
   rowid, and rows by key.
6. **Duplicate label sets after mapping to `other`** (high, accepted). Plugins merge on collision (max utilization, min
   reset). The renderer also drops a duplicate series and logs once, as a guard.
7. **`-shm` written by a read-only reader; memory** (high, accepted). Measure before claiming:
   - a test opens a WAL DB `mode=ro` under strace, in a read-only directory and with `-shm` missing;
   - the result goes in `specs/sources.md`.
   Connections use `cache_size=-256`, `mmap_size=0` and one connection each, closed between cycles.
8. **Non-scalar `json_extract` could carry a secret** (medium, accepted). A listed extract is wrapped so only scalar JSON
   types come back; text is length-capped and mapped to a vocabulary. Identifiers are quoted and matched exactly.
   `threads` is dropped from the list, since `codex-state` is not built.
9. **`message.data` whole vs `json_extract`** (medium, accepted). `specs/sources.md` wins: `json_extract` of listed paths
   only. The plan's acceptance line is corrected in the PR.
10. **Contract conflicts** (medium, accepted):
    - agy `weekly` stays `weekly`, by the contract's window rule. Codex minutes become `5h`, `7d`, or `<n>h`/`<n>d`/`<n>m`.
    - Codex `limit_id="codex"` maps to `scope="all"`.
    - The contract documents that the age alert and `cache_age` mean something different for passive sources.
    - **Provider balances are deferred.** A per-account `unit` label breaks "no unit in a label value", and balances
      are money, which the fleet's rules keep off its pages. That is the maintainer's call. It is not a reason to bend
      the contract, so item 5 waits for a ruling.
11. **Router cardinality** (medium, accepted). Connections are capped per profile at 50, with the rest in
    `account="other"`. Models are capped per profile across connections.
12. **agy details dropped** (high, accepted):
    - interval 30 m, floor 15 m;
    - calls serialised, backoff on non-zero exit, a health counter for unknown buckets;
    - its own timeout, SIGTERM then a wait, never SIGKILL first;
    - an explicit `account` is required;
    - the source disables itself unless the output says `command.name == "usage"`, `num_turns == 0` and `total_tokens == 0`.
13. **Shared host-wide network gate** (medium, accepted). Sources may name a gate group. Claude's private endpoint keeps
    the `vendor` group and agy gets its own, so a 429 from one never pauses the other.
