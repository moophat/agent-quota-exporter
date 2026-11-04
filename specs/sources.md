# Sources

What each plugin reads, how, how often, what it must never touch, and what is out of scope. Evidence level is marked:
**verified** (observed read-only on a real install while writing this repo), **documented** (vendor docs or the
binary's own help), **unverified** (reported by others, not run). A builder must re-verify unverified items.

General rules for every source:

- Open files and databases **read-only**. SQLite: `file:<path>?mode=ro` (a live WAL database must not be opened
  `immutable`, and never copied with a full backup on every poll). Query **explicit column lists**, never `SELECT *`.
- Read incrementally (by id, sequence number or byte offset), keep the offset in the exporter's own state dir.
- Parse structural fields only; drop every content field at the parser.
- A credential file is opened **only** by a plugin that declares it, for **one** request's lifetime, and its value is
  never logged or labelled. Prefer reading just the expiry timestamp when that is all a metric needs.
- Never write to any agent directory. (The one accepted exception: SQLite read-marks in a live writer's existing
  `-shm`, `specs/decisions.md` D8.) Never invoke an agent CLI that can start sessions or refresh logins, unless the
  source below says so and it is opt-in.

## Claude Code (harness `claude-code`, provider `anthropic`)

A *profile* is one `CLAUDE_CONFIG_DIR` (default `~/.claude`; accounts are separated by separate config dirs).

| Source | What | How | Cadence | Evidence |
|---|---|---|---|---|
| `claude-transcripts` | per-message token usage and model | tail `<config>/projects/**/*.jsonl` (including `subagents/*.jsonl`) by byte offset; from each `assistant` line take `message.id`, `message.model`, `message.usage.{input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens, cache_creation.ephemeral_1h_input_tokens}`, `timestamp`; skip `model == "<synthetic>"`; dedupe by `message.id` (keep the highest `output_tokens`, a message can be logged more than once while streaming) | 1 to 5 min, incremental | verified |
| `claude-sessions` | live session count and state | read `<config>/sessions/<pid>.json`: `status`, `updatedAt`, `startedAt`, `pid` (check the pid is alive). Count by `status`. Do not read `cwd`/`name` | each scrape (cheap, local) | verified |
| `claude-account` | account identity, plan | `<config>/.claude.json` `oauthAccount` object: account uuid/email domain only if needed to derive the alias hash; **never** other keys of that file. `<config>/.credentials.json` `claudeAiOauth`: `expiresAt`, whether `accessToken` is a non-empty string (never its value), and `subscriptionType` for the `plan` label (bounded to `pro`, `max`, `team`, else `unknown`) | at start, on change | verified (file shape) |
| `claude-oauth-usage` | quota windows | `GET https://api.anthropic.com/api/oauth/usage` with header `anthropic-beta: oauth-2025-04-20` and the profile's current access token from `<config>/.credentials.json`. Response objects `five_hour`, `seven_day` (each `utilization` in percent, `resets_at` ISO time), other top-level objects with a numeric `utilization` (separate model-family limits), and `limits[]` (`kind`, `percent`, `severity`, `is_active`). See `fixtures/claude/oauth-usage-response.json` | **at most once per 15 min per account**; 60 min after a 429; stop for the profile after two consecutive 401s until the credential file changes | verified (field names), **undocumented endpoint** |

Rules specific to `claude-oauth-usage`:

- **Opt-in per profile** (`quota.claude_oauth_usage: true`). The endpoint is private and undocumented: it can change or
  rate-lock without notice (reported in public issue trackers; a lock is shared by IP, so a burst of bad requests from
  one host locks every profile on it).
- The access token goes from the file to the request header in memory and is never printed. Send the header through a
  mechanism that cannot be rewritten or echoed by logging layers; a malformed `Authorization` value that reaches the
  endpoint counts toward the rate lock.
- **Never refresh.** On 401 the token is stale (an idle client stops refreshing; the next real session revives it). Report
  `agent_source_errors_total{reason="auth_expired"}` and wait for the credential file's mtime or expiry to change.
- One request per **account**, not per profile, when two profiles hold the same grant (they should not; see the profile
  principle). If detected, warn once and query only one.
- Never-read columns/files: `.credentials.json` other than `accessToken`, `expiresAt` and `subscriptionType`; `.claude.json` other than the
  allowed `oauthAccount` fields; `history.jsonl` and all `message.content`, `toolUseResult`, prompts.

## Codex (harness `codex`, provider `openai`)

A *profile* is one `CODEX_HOME` (default `~/.codex`). Observed layout (names only) is in `fixtures/codex/layout.txt`.

| Source | What | How | Cadence | Evidence |
|---|---|---|---|---|
| `codex-rollouts` (on by default, passive) | quota windows, credits, per-call tokens and model | tail `<CODEX_HOME>/sessions/YYYY/MM/DD/rollout-*.jsonl` by byte offset. Only two line kinds are decoded: `type=="turn_context"` (`payload.model` only) and `type=="event_msg"` with `payload.type=="token_count"` (`timestamp`, `payload.info.{total_token_usage,last_token_usage}`, `payload.rate_limits.{limit_id, primary, secondary, credits}`). See `fixtures/codex/rollouts/` and `fixtures/codex/home/` | the usage interval (1 min), incremental | verified (shape, Codex CLI 0.163.0, 2026-11-04 on-prem); token arithmetic **documented** from the Codex source (below) |
| `codex-rate-limits` | primary and secondary window `usedPercent`, `resetsAt`, reset credits | JSON-RPC over stdio to `codex app-server`: `account/read`, `account/rateLimits/read` (shape in `fixtures/codex/rate-limits-response.json`, synthetic) | at most once per account per 15 min | documented, **unverified** (the node used for fixtures is not logged in). **Not built** (v0.2 plan item 7: only if the passive reading proves too stale) |
| `codex-state` | sessions, usage | `<CODEX_HOME>/state_*.sqlite` (and `logs_*.sqlite`): read-only, explicit columns | 1 to 5 min | **Not built** in v0.2; `threads` holds prompt-adjacent text columns |
| `codex-auth` (presence only) | logged-in state | check that `<CODEX_HOME>/auth.json` exists and its mtime; do not read its contents | each scrape | verified (name) |

Rules specific to `codex-rollouts`:

- **Token types.** Codex's `TokenUsage` copies the Responses API usage: `input_tokens` *includes*
  `cached_input_tokens` and `cache_write_input_tokens`, and `output_tokens` *includes* `reasoning_output_tokens`
  (`total_tokens` = input + output). Source, `openai/codex` `main` read 2026-11-04: `codex-rs/codex-api/src/sse/responses.rs`
  (`From<ResponseCompletedUsage> for TokenUsage` maps `input_tokens_details.{cached_tokens, cache_write_tokens}` and
  `output_tokens_details.reasoning_tokens`; its test `parses_cache_write_token_usage` has input 100 = cached 40 + cache
  write 60, total 110 = input 100 + output 10 with reasoning 5) and `codex-rs/protocol/src/protocol.rs`
  (`TokenUsage::non_cached_input()` = input − cached). So the contract types are, each floored at 0:
  `input` = input − cached − cache write, `cache_read` = cached, `cache_write` = cache write,
  `output` = output − reasoning, `reasoning` = reasoning.
- **Counting.** A `token_count` counts its `last_token_usage` only when its `total_token_usage` differs from the
  previous total in the same file (Codex writes the same event twice); `agent_requests_total{outcome="ok"}` +1 per
  counted event. The model is the file's latest `turn_context` model (kept per file), `unknown` before one, capped per
  profile like Claude. No replay: files present at the first collect start at their end; files appearing later are read
  from the start, skipping lines older than their first sighting minus one hour (history copied on fork or resume).
- **Quota is a gauge, one reading per scope**: `scope` = `all` for `limit_id` `codex` or none, the id itself if it
  matches `^[a-z0-9_-]{1,32}$`, else `other`. Each scope keeps the `token_count` with non-null `rate_limits` whose own
  `timestamp` is newest for that scope across all files, so Codex alternating between limits never hides one of them.
  A null `rate_limits` never replaces a reading. At most 8 scopes per profile; a new one beyond that evicts the scope
  whose newest reading is oldest (not an error). At the first collect the tails (last 1 MiB) of the 20 newest files by
  mtime are searched the same way, so an idle profile has its readings at once; they are persisted.
  `primary`/`secondary` give `agent_quota_utilization_ratio` (`used_percent` / 100) and
  `agent_quota_reset_timestamp_seconds`; `window` from `window_minutes` (300 → `5h`, 10080 → `7d`, else `<n>d`, `<n>h`
  or `<n>m` by the largest whole unit, missing → `other`). Two windows of a scope with the same labels merge (max
  utilization, earliest reset).
- **Passive staleness** (contract, "Passive sources are the one exception"), per scope: a window is served until its
  `resets_at` passes; one without `resets_at` for `stale_after` from its reading. `agent_quota_observed_timestamp_seconds`
  has no `scope` label: it is one series per profile, the newest reading time among the scopes still served, and is
  absent when none is.
- **Credits**, per scope: `agent_quota_credits_available` = `has_credits || unlimited`; `agent_quota_credits_balance` =
  `balance` (a string in Codex, a number accepted) when not unlimited and numeric. A reading with null `credits` keeps
  the scope's previous credits; credits are served at most until the longest window of their own reading resets
  (reading + 7 days with none).
- Never decoded or kept: any other line type (`response_item`, `session_meta`, other `event_msg`), and in the two
  decoded kinds every field not listed above (`cwd`, instructions, `limit_name`, message text). A relevant line over
  4 MiB is skipped and counted as a parse error; an irrelevant line is never buffered.
- Missing or linked `sessions/` is `not_found` / `permission`, never zero values. Links at any level are refused.
- Not exported: session counts (no live-session signal with a pid), Codex `error` events (no real sample of their shape yet).

Rules specific to `codex-rate-limits`:

- **Opt-in and flagged risky.** Spawning `codex app-server` starts the vendor's own client, which may refresh its own
  token as part of normal operation. That is the real client doing what it always does, but it makes the exporter a
  trigger for a refresh the exporter did not initiate. Phase 3 must measure whether a single `rateLimits/read` ever causes
  a refresh; until then it is off by default and never run on an interval shorter than 15 minutes.
- Run with the profile's `CODEX_HOME` set in the child environment; never share one `CODEX_HOME` between profiles.
- Never read: `auth.json` contents, anything under `memories_*`, `queue_*`, `goals_*` (prompt-adjacent text).

## opencode (harness `opencode`, provider as reported per message)

A *profile* is one opencode instance with its own data dir (`<data>/opencode/opencode.db`) and, if running, its HTTP port.
The deployment norm is several instances, one per lane/account.

| Source | What | How | Cadence | Evidence |
|---|---|---|---|---|
| `opencode-db` | per-message tokens, cost, model, provider, error | `opencode.db` SQLite, read-only. `message.data` is JSON with `role`, `providerID`, `modelID`, `cost`, `tokens{total,input,output,reasoning,cache{read,write}}`, `finish`, `time`; pull these with `json_extract` so no other part of the JSON leaves SQLite. Incremental by rowid, plus re-reads by id of messages still in flight (below). Schema in `fixtures/opencode/opencode.schema.sql` | usage interval (default 60 s, floor 30 s) | verified |
| `opencode-http` | live sessions, connected providers | `GET /session` on the instance (tokens, cost, model per session); `GET /provider` for connected providers: **read only the provider ids and the `connected` list** | each scrape, cached 30 s | documented, partly verified |

How `opencode-db` reads and counts (v0.2):

- **What it selects.** Table `message` only: the rowid, `id`, and `json_extract` of `data` at `$.role`,
  `$.providerID`, `$.modelID`, `$.cost`, `$.tokens.input`, `$.tokens.output`, `$.tokens.reasoning`,
  `$.tokens.cache.read`, `$.tokens.cache.write`, `$.time.completed`, `$.error.name` and `$.error.data.statusCode`.
  `message.data` is never selected whole. The error's `message`, `responseBody` and headers are never read. No other
  table is listed, including `session` (its roll-ups are not needed). Any other expression fails before SQL is built
  (`internal/opencode` tests).
- **When a message counts.** opencode inserts an assistant row when the call starts and updates the same row while it
  streams and when it ends. A row is complete when `time.completed` is set, or when `error.name` is set, since an error
  ends the call. Only complete `assistant` rows count, each exactly once, with the values of the complete row. New rows
  come from a rowid cursor. An incomplete row goes into a persisted pending set and is re-read by id each cycle until it
  completes. The set is capped at 2000, dropping the oldest, and a row still incomplete after 24 h is dropped (its
  writer died). Each drop counts as `agent_source_errors_total{reason="other"}`. A row whose `data` is not JSON counts
  as `reason="parse"`.
- **No replay.** The first collect for a profile puts the cursor at the current last rowid. Existing rows are history
  and are not counted, including calls still in flight then. Rows written later always count, so an exporter outage
  loses nothing. If the last rowid falls below the cursor (the database was replaced), the cursor moves down to it,
  again without replay.
- **Tokens** map one to one, and nothing is subtracted: `tokens.input` → `input`, `output` → `output`, `reasoning` →
  `reasoning`, `cache.read` → `cache_read`, `cache.write` → `cache_write`. opencode already stores input without
  cached tokens (`nonCachedInputTokens`) and output without reasoning (`visibleOutputTokens`). Source:
  `packages/core/src/session/usage.ts` in opencode `dev`, read 2026-11-04. `tokens.total` is not read.
- **Requests and errors.** `agent_requests_total{outcome="error"}` when `error.name` is set, else `ok`. `code`:
  `ProviderAuthError` → `auth`. `APIError` maps by `statusCode`: 429 → `rate_limited`, 401 or 403 → `auth`, 503 or
  529 → `overloaded`. Everything else is `other`, including a user abort (`MessageAbortedError`).
- **Labels.** `provider` = `providerID` lowercased if it matches `^[a-z0-9._-]{1,48}$`, else `other`. `model` =
  `modelID` lowercased under the Claude model rule: `unknown` if missing, `other` if unsafe. Each profile has at most
  `max_models_per_profile` (provider, model) pairs with their own series. Further pairs count into `model="other"` under
  their provider, and a pair unused for 24 h gives up its slot. `account` = the profile's configured `account`, else
  its alias.
- **Idle database.** If the database is in WAL mode and has no `-wal` or `-shm` file, no opencode has it open. It is
  read with `immutable=1`, which opens only the main file read-only and creates nothing (strace, 2026-11-04), so the
  final rows a closing opencode checkpointed are still counted. With exactly one sidecar missing (an odd state) the
  persisted counters are served for that cycle. Busy or locked: `reason="timeout"`, retried next
  cycle. Missing: `reason="not_found"`. The connection is opened and closed every cycle.

Known data-quality traps (from observed instances):

- A proxy-fronted lane can under-report: a large share of its calls show zero usage. Export what is stored and let the
  consumer judge; do not "fix" it.
- Cost can be `0` for subscription providers. `agent_cost_usd_total` is only exported when a provider reports a non-zero
  cost at least once.
- Provider ids stored in historical rows can differ from the live config after a rename; labels follow the stored value.

Never-read (hard blocklist, enforced by the query allow-list): tables `account`, `control_account`, `credential`
(`access_token`, `refresh_token`, `value`), `permission`, `part` (message text), and `/provider` response keys
`key` and `options`.
- Known limit: `message` has a text primary key, so its rowid is implicit. If the newest rows are deleted (a
  session removed) and new ones arrive before the next collect, SQLite can reuse rowids at or below the cursor and
  those rows are not counted. This can only undercount, never double count.

## agy (harness `agy`, provider `google`)

A *profile* is one `HOME` per account. agy profiles are never discovered: they exist only when declared in config.
Local state holds no quota, but the official client has a quota surface (this corrects the earlier "no quota surface"):
`agy -p /usage --output-format json` answers with quota buckets, using 0 tokens and no model turn (agy CLI 1.5.0, one
account, read twice on-prem on 2026-11-04).

| Source | What | How | Cadence | Evidence |
|---|---|---|---|---|
| `agy-usage` | quota windows | spawn the official client with the fixed argv `agy -p /usage --output-format json`; read `command.data.groups[].buckets[]`: `id`, `window`, `remaining_fraction`, `reset_time`. See `fixtures/agy/usage-command.json` | default every 30 min, floor 15 min; 60 min after a non-zero exit or a timeout; 24 h after the guard below trips | verified (field names, 0 tokens), agy CLI 1.5.0 |

Rules specific to `agy-usage`:

- **Opt-in per profile** (`quota.agy_usage: true`), off by default.
- **An explicit `account` is required.** The same login can sit on many machines, so the operator reads each account
  from one host only by declaring `account` on that host's profile. Without it the source spawns nothing and reports
  `agent_source_up 0` with `agent_source_errors_total{reason="other"}` on every attempt. The `account` label is the
  configured alias; nothing is read from the agy home to derive it.
- **The exporter never opens a file in the agy home and never holds a token.** It runs the client the way a person would:
  `exec` with no shell, the binary found with `PATH` lookup (must resolve to an absolute path), working directory and
  `HOME` = the profile, environment exactly `HOME`, `PATH` (the exporter's own, `/usr/local/bin:/usr/bin:/bin` if
  unset) and `LANG=C`, stdin from `/dev/null`,
  stdout capped at 256 KiB (more is a `parse` error), stderr discarded and never logged.
- **The client runs as the owner of the profile `HOME`, never as anyone else.** A login refresh rewrites the token file;
  run as another user (root, say) it would leave that file owned by the wrong user and could lock the owner out of
  their own agy. Before each run the exporter `lstat`s the `HOME`: it must be a real directory, not a link (else
  `reason="permission"`, nothing spawned; a missing `HOME` is `not_found`). An exporter running as root runs the client
  with the `HOME`'s owner uid and group gid and no supplementary groups (root only for a root-owned `HOME`). An exporter
  running as any other user runs the client only for a `HOME` it owns itself; for any other `HOME` it spawns nothing and
  reports `reason="permission"` on every attempt. So a dedicated, unprivileged exporter user cannot read agy quota for
  other users' profiles: run the exporter as root, or as the profile's own user. The client binary must be executable
  by the `HOME`'s owner (an unreadable one is `reason="permission"`).
- **Token refresh is the client's.** A run with an expired access token refreshes it and rewrites the token file, as any
  agy run does; two runs with a live token changed nothing (on-prem, by hash and mtime only). So each poll can trigger the
  client's normal refresh. Whether the refresh token itself rotates is re-checked on-prem on an rc before `v0.2.0`; if it
  rotates, the source stays out of the release.
- **Timeout:** 30 s per run. The client runs in its own process group; on timeout the group gets `SIGTERM`, then 5 s to
  finish writing its token file, then `SIGKILL`. Never `SIGKILL` first. After every run, timeout or not, whatever is
  left of the group (a child that outlived the client, for instance one still holding its stdout) gets the same
  `SIGTERM`, grace, `SIGKILL`.
- **One client at a time** per exporter, across all profiles. agy has its own gate group (`agy`) for spacing and backoff,
  so a backoff on Claude's endpoint never pauses agy and the reverse. `sources.agy-usage.backoff_429` sets the backoff after
  a non-zero exit or timeout (floor 60 min).
- **Zero-cost guard.** The output is used only if `command.name == "usage"`, `num_turns == 0` and `usage.total_tokens == 0`,
  each present. Anything else means the client changed in a way that may spend model turns: no sample is taken from that
  output, the error is `reason="other"` and the source pauses for 24 h.
- **Mapping:** `agent_quota_utilization_ratio` = `1 - remaining_fraction` (floored at 0; a bucket without it is absent,
  never zero); `agent_quota_reset_timestamp_seconds` from `reset_time` (RFC 3339); `agent_quota_observed_timestamp_seconds`
  = the time of the successful run. `window` is the bucket's `window` lowercased (`5h`, `weekly`: named windows keep the
  vendor word) if it matches `^[a-z0-9]{1,16}$`, else `other`. `scope` is the bucket id before its first `-`: `gemini` or
  `3p`, else `other`. Buckets that share a label set after that mapping merge (highest utilization, earliest reset).
- **Unknown bucket ids** are served under `scope="other"` and each one counts in
  `agent_source_errors_total{reason="parse"}` while the read still succeeds: the output no longer matches the known
  vocabulary, and the contract's existing bounded counter shows it without a new metric or label value.
- **Staleness:** a live source, so the contract's `stale_after` applies. The last good reading is persisted and served
  after a restart until stale.
- **`--once`** never spawns the client unless `--once-with-quota` is given, and then only if its gate allows.
- Never-read: every file in the agy home; in the output, `response`, every `name` and `description`, and every field not
  listed above.

## 9router (harness `9router`, provider as recorded per row)

A *profile* is one `.9router` directory. Router profiles are never discovered: they exist only when declared in config,
since enabling a named product is the operator's choice. Its database `<dir>/db/data.sqlite` is live, usually in WAL
mode, and can pass 900 MB.

| Source | What | How | Cadence | Evidence |
|---|---|---|---|---|
| `router-9router` | requests, tokens and cost per connection; connection health | `data.sqlite` read-only through `internal/sqlro`, one connection opened and closed per cycle. `usageHistory`: `id`, `provider`, `model`, `connectionId`, `promptTokens`, `completionTokens`, `cost`, `status`, rows after a persisted `id` cursor (primary key, so no scan), at most 5000 rows per cycle; a backlog is read over the next cycles. `providerConnections`: `id`, `provider`, `name`, `isActive`, `email` (only with `account_label: email`), and from the JSON column `data` only the scalars `$.testStatus` and `$.rateLimitedUntil`; the whole table every cycle (at most 1000 rows). Schema in `fixtures/router/router.schema.sql`, rows in `fixtures/router/rows.sql` | `defaults.usage_interval` (floor 30 s) | verified (schema, on-prem 2026-11-04); value words documented (9router source, 2026-11-04) |

Mapping:

- **Counters:** `agent_tokens_total{type="input"}` = `promptTokens`, `{type="output"}` = `completionTokens`.
  9router fills `promptTokens` from the upstream's `prompt_tokens` or `input_tokens`, so for an OpenAI-shaped upstream
  `input` includes cached input; the router keeps no separate column for it. `agent_requests_total{outcome}`: `ok` when
  `status` is NULL, empty, `ok`, `success` or an HTTP 2xx code (9router writes `ok` unless a caller sets another value,
  and reads a missing one as `ok`), else `error`. 9router mostly records successful calls, so `error` undercounts.
  `agent_cost_usd_total` = sum of `cost` (negative read as 0); it is exported only once a non-zero cost has been seen for
  the series, since 9router writes 0 for a model it has no price for.
- **First start** sets the cursor to the newest `id`: nothing already in the table is counted. If the newest `id` falls
  below the cursor (the table was replaced or its newest rows deleted), the cursor moves down to it without counting.
- **`account`** = the connection: its `name` lowercased if it matches `^[a-z0-9._-]{1,48}$`, else (with
  `account_label: email` only) its `email` lowercased if well formed, else `acct-` + the first 8 hex digits of
  sha256(connection id), the same rule as `claude-code`. A label already taken in the profile (two connections with
  one name), and the reserved words `other` and `unknown`, fall through to the next candidate. A row without a
  `connectionId` is `account="unknown"`; one whose connection is gone is `account="other"`.
- **Connection cap:** the 50 connections seen first in a profile have their own label; the rest count into
  `account="other"` and get no health series. Deleting a labelled connection frees its slot for the next one; the
  deleted label's counters end (a label that comes back restarts from 0, a counter reset).
- **`provider`** = the row's (or connection's) provider lowercased if it matches `^[a-z0-9._-]{1,48}$`, else `other`;
  empty is `unknown`. **`model`** is lowercased and bounded as for `claude-code`, capped per profile across all
  connections at `metrics.max_models_per_profile`; a model past the cap counts into `model="other"`, and one unused for
  a day gives its slot to a new one.
- **Health:** `agent_router_connection_up` = 1 if `isActive` is 1, `testStatus` is a passing word and the connection is
  not rate-limited now, else 0. `testStatus` words follow 9router's own code: `error`, `expired`, `unavailable` mean
  the last test failed; `active`, `ok`, `success`, `unknown`, `testing` and none do not. Any other word reads as failed
  and counts in `agent_source_errors_total{reason="parse"}` every cycle, so a new upstream state shows instead of
  passing silently. `rateLimitedUntil` is an RFC 3339 time (what 9router writes); a Unix time in ms (above 1e11) or s,
  number or numeric string, is accepted too; anything else is a parse error and no limit.
  `agent_router_rate_limited_until_timestamp_seconds` is that time while it is in the future, absent otherwise. A
  `data` value that is not JSON reads as no test result and no limit. `errorCode` and `lastErrorAt` are not read: the
  router's own `testStatus` already reflects them.
- **Idle database:** a WAL database without `-wal` or `-shm` has no writer, so it is read with `immutable=1`, which
  creates nothing in the router's directory. With exactly one sidecar missing, the cycle serves the persisted counters
  and the persisted health, re-evaluated against the current time (a rate limit that ended reads up).
- **Errors:** a missing database is `not_found`; a lock held by the router is `timeout` (no wait beyond 200 ms; retried
  next cycle). Either way the previous values are served until stale.
- **Measured:** 5000 rows per cycle take about 20 ms with a 3 MiB peak heap (`BenchmarkCycle`, 200k-row table, a
  4-vCPU cloud VM; the read itself is single-threaded). On-prem has not measured it against a real router database.

Never-read (enforced by the query allow-list and tested): `usageHistory.apiKey`, `usageHistory.tokens`,
`usageHistory.meta`, `usageHistory.endpoint`, `providerConnections.data` as a whole and every path in it except the two
above (it holds `accessToken`, `refreshToken`, `idToken`, `apiKey`), `providerConnections.email` unless
`account_label: email`, and every other table.
- Known limit: labels are recomputed each cycle. If a connection is deleted or renamed and another connection takes
  its label, the newcomer's usage continues that label's counters instead of starting from zero (counters stay
  monotonic; the attribution moves). A connection name that looks like a credential (a common key prefix, or a run
  of 20 or more letters and digits) is never used as a label; the hash of its id is used instead.

## Other providers (later, by plugin)

| Provider | Official surface | Scope |
|---|---|---|
| Anthropic API keys (billing) | Admin usage and cost reports API | in scope as an optional provider plugin; hourly-bucket data, daily-lagged; needs an admin key (a secret the operator provides via env or file reference) |
| OpenAI API keys (billing) | organization usage/costs API | same pattern |
| OpenRouter, DeepSeek, GLM and similar | balance endpoints with an API key | not built (`specs/decisions.md` D9: prices are not known in real time) |
| Google AI subscription windows | the agy client's own `/usage` command | in scope as `agy-usage` (opt-in), see the agy section; nothing calls the private endpoint itself |

Keys for these plugins are referenced from config as `env:NAME` or `file:/path` and are never inlined in the config
file.

## Out of scope

- Anything that requires **refreshing, minting or rotating an OAuth token**, or logging a client in or out.
- Calling a model (no inference to "test" an account; no tokens spent by the exporter).
- Reading any prompt, completion, tool input/output or file content from any agent's state.
- Reading any credential file beyond the one value or timestamp a declared plugin needs.
- Router and gateway internals other than 9router (see its section). Each would be its own plugin, always behind a
  column allow-list, since their tables hold provider tokens and API keys.
- Host CPU/memory/disk metrics: use `node_exporter`.
- Writing to or modifying any agent configuration.
