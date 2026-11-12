# Sources

What each plugin reads, how, how often, what it must never touch, and what is out of scope. Evidence level is marked:
**verified** (observed read-only on a real install while writing this repo), **documented** (vendor docs or the
binary's own help), **unverified** (reported by others, not run). A builder must re-verify unverified items.

General rules for every source:

- Optional private structural history follows [event-contract.md](event-contract.md). Its field
  allowlists and freshness/failure requirements supersede metrics-only exclusions below. Current
  implementation details remain evidence of existing behavior, not proof the new event lane is built.
  Capture original IDs/tags/raw model/window names before metric caps; never copy whole objects.

- Open files and databases **read-only**. SQLite: `file:<path>?mode=ro` (a live WAL database must not be opened
  `immutable`, and never copied with a full backup on every poll). Query **explicit column lists**, never `SELECT *`.
- Read incrementally (by id, sequence number or byte offset), keep the offset in the exporter's own state dir.
- Parse structural fields only; drop every content field at the parser.
- A credential file is opened **only** by a plugin that declares it, for **one** request's lifetime, and its value is
  never logged or labelled. Prefer reading just the expiry timestamp when that is all a metric needs.
- Never write to any agent directory. (The one accepted exception: SQLite read-marks in a live writer's existing
  `-shm`, `specs/decisions.md` D8.) Never invoke an agent CLI that can start sessions or refresh logins, unless the
  source below says so and it is opt-in.

## Strata (harness `strata`, source `strata`)

Explicit opt-in with top-level `strata: [{name: lane-a, base_url: "http://127.0.0.1:8080", interval: 30s}]`;
absent/empty by default, no discovery. Each entry names a unique lane. Default poll interval 30 s, hard floor
5 s. Only read-only GET `/slots` and `/metrics` (JSON, not Prometheus) below the configured base URL;
no credentials, token reads, inference calls or redirects. URLs must have no credentials, query or fragment.
Each response is bounded to 1 MiB and reads to the scheduler's 10 s timeout. Each lane has its own network
gate, 5 s minimum spacing and 1 min 429 backoff, independent of vendor sources. Other failures retry at the
normal jittered interval. The scheduler persists attempt/backoff gates and emits standard source health.

Allowlist: slot `is_processing` and list length; `live.queued`, `totals.requests`,
`engine.{model,images,max_context}`. Ignore per-request histories, prompts and conversation cache data.
Map to the five `agent_lane_*` families in metric-contract.md; missing/null numeric values are absent,
never synthesized zero. A failed endpoint fails the poll atomically; last-good samples age out at
`max(defaults.stale_after, 2*P + D + 15s)` without a failed poll refreshing them. No persisted lane samples
are restored after restart. Evidence: supplied captured response fixtures in `internal/strata/testdata/`;
runtime tested with a local HTTP server, no live server verification.

## Claude Code (harness `claude-code`, provider `anthropic`)

A *profile* is one `CLAUDE_CONFIG_DIR` (default `~/.claude`; accounts are separated by separate config dirs).

| Source | What | How | Cadence | Evidence |
|---|---|---|---|---|
| `claude-transcripts` | structural per-message usage and tags | tail `<config>/projects/**/*.jsonl` including subagents by byte offset; preserve event-contract usage IDs/timestamps/session/parent/agent/sidechain/cwd/branch/model/version/entrypoint/skill/tier/speed and token buckets where present, never content. Retain revisions; named views define deduplication; skip synthetic models | target <=60 s capture, bounded recovery with explicit backlog | existing token shape verified; new event capture requires implementation verification |
| `claude-sessions` | live session count and state; with `metrics.per_session`, one series group per live session | read `<config>/sessions/<pid>.json`. Counts (always): `status`, `pid`, `procStart`, `pidDomain`; a session is live only if the pid exists, field 22 of `/proc/<pid>/stat` (parsed after the last `)`) equals `procStart` when the file has one, and the whole `pidDomain` string (`linux:<machine-id>:<pid namespace>`, the namespace part alone is the same on every host) equals this host's when the file has one. Per session (opt-in): also `name`, `kind`, `spare`, `entrypoint`, `tmux`, `waitingFor`, `startedAt`, `statusUpdatedAt` (epoch milliseconds). Skipped per session: `spare: true` and any `kind` other than `interactive` or `bg`. `waitingFor` is mapped through an exact allowlist (`permission prompt`, `input needed`, `dialog open`, `sandbox request`, `worker request`, `goal proposal`) to a fixed reason; the harness's own set is closed (no dialog text reaches it) and the allowlist keeps it that way if a new string appears, which reads `other`. `name` becomes the `session` label after sanitizing: **in practice it is the project folder name plus a short suffix** (`nameSource: derived`), so the label usually shows a project folder name. `tmux` is `session:@window.%pane` with no socket: only the session name is kept, and the same session name on two sockets is not told apart. Never read: `cwd`, `sessionId`, sockets, peer fields. A session file can outlive its process, and `entrypoint: sdk-cli` sessions include background observers a tool starts for itself: they count in `agent_sessions` (as before) and carry `entrypoint="sdk"` in the per-session series. A failed read of the directory drops the cached series (source down), it does not keep the last list | each scrape (cheap, local) | verified for the file shape (2026-11); the `waiting` state and its `waitingFor` strings are read from the harness's binary, not observed on a live file in every case |
| `claude-account` | account identity, plan and historical binding | shared public metric account label (alias, existing UUID short hash, or configured email) and bounded plan label; event metadata uses the exact identity allowlist below, never other file keys or token values | local metadata target 60 s, on change | existing file shape verified; history requires implementation verification |
| `claude-oauth-usage` | quota windows | `GET https://api.anthropic.com/api/oauth/usage` with header `anthropic-beta: oauth-2025-04-20` and the profile's current access token from `<config>/.credentials.json`. Response objects `five_hour`, `seven_day` (each `utilization` in percent, `resets_at` ISO time), other top-level objects with a numeric `utilization` (model-family limits, and windows of unstated length), and `limits[]` (`kind`, `percent`, `severity`, `is_active`). See `fixtures/claude/oauth-usage-response.json` | **at most once per 15 min per account globally**; >=60 min after a 429; two-401 holder exclusion as below | verified (field names), **undocumented endpoint** |

Rules specific to `claude-oauth-usage`:

- Current-series eligibility follows the canonical [Claude freshness policy](event-contract.md#claude-freshness-policy),
  whose margin covers the effective poll interval, normal gate delay and read completion.
- **Opt-in per profile** (`quota.claude_oauth_usage: true`). The endpoint is private and undocumented: it can change or
  rate-lock without notice (reported in public issue trackers; a lock is shared by IP, so a burst of bad requests from
  one host locks every profile on it).
- The access token goes from the file to the request header in memory and is never printed. Send the header through a
  mechanism that cannot be rewritten or echoed by logging layers; a malformed `Authorization` value that reaches the
  endpoint counts toward the rate lock.
- **Never refresh.** On 401 the token is stale (an idle client stops refreshing; the next real session revives it). Report
  `agent_source_errors_total{reason="auth_expired"}`. Record each attempt; after two 401s exclude that
  holder for an hour. Another holder requires the same global account gate; credential change clears
  auth-specific backoff only, never a 429 or the global gate.
- **Mapping to series** (`agent_quota_utilization_ratio` = `utilization` / 100, `agent_quota_reset_timestamp_seconds`
  only when `resets_at` is given; a null object or null `utilization` is absent, never zero):
  - `five_hour` → `window="5h"`, `seven_day` → `7d`, scope `all`; `five_hour_X` / `seven_day_X` → scope `X`.
  - Any other top-level object with a numeric `utilization` (seen: `iguana_necktie`, `extra_usage`) → `window="other"`,
    scope = the key. Its length is not given and not guessed. At most 8 such keys per response, the first by key name,
    so a vendor adding keys cannot grow the series; a key over 48 characters is dropped. Two keys that normalise
    to the same scope both stay: the later by key name gets the scope cut to 39 plus `_` and 8 hex digits of sha256(key),
    re-hashed with a salt if even that scope is taken (at most 8 tries, else the key is dropped). `extra_usage` is the paid
    allowance used past the subscription limits, a credits figure rather than a quota window; only its `utilization`
    ratio is exposed, under the same rule, never an amount (D9).
  - `limits[]`: `kind` `session` → `5h/all`, `weekly_all` → `7d/all`, `weekly_X` → `7d/X` (so `weekly_scoped` →
    `7d/scoped`). Its numeric `percent` gives the ratio of that window and scope, whether the limit is active or not,
    unless a top-level object already gave it (that one is more precise and carries the reset). Only an active entry
    gives `agent_quota_limit_info` with its `severity`.
- One request per **account**, not per profile, when two profiles hold the same grant (they should not; see the profile
  principle). If detected, warn once and query only one.
- Identity history additionally allowlists exactly `claudeAiOauth.{expiresAt,refreshTokenExpiresAt,
  subscriptionType,rateLimitTier}` and `oauthAccount.{emailAddress,organizationName,organizationType,
  billingType,seatTier,hasExtraUsageEnabled,organizationRateLimitTier,userRateLimitTier}`, plus credential
  mtime. See event-contract for destinations/nulls and the shared public account label policy; no whole-object copy.
  Configured email labels and email-bearing allowlisted identity metadata are retained without identity-only email redaction.
  Access token remains request-local only. Everything else
  in those files, `history.jsonl`, `message.content`, `toolUseResult` and prompts remains excluded.
- All numeric quota windows and all reduced limits survive privately in events, even when inactive,
  unknown, or over metric caps. Passive cache time is payload observation time, not file scan time.

## Codex (harness `codex`, provider `openai`)

A *profile* is one `CODEX_HOME` (default `~/.codex`). Observed layout (names only) is in `fixtures/codex/layout.txt`.

| Source | What | How | Cadence | Evidence |
|---|---|---|---|---|
| `codex-rollouts` (on by default, passive) | quota windows, credits, per-call tokens and model | tail `<CODEX_HOME>/sessions/YYYY/MM/DD/rollout-*.jsonl` by byte offset. Only two line kinds are decoded: `type=="turn_context"` (`payload.model` only) and `type=="event_msg"` with `payload.type=="token_count"` (`timestamp`, `payload.info.{total_token_usage,last_token_usage}`, `payload.rate_limits.{limit_id, primary, secondary, credits}`). See `fixtures/codex/rollouts/` and `fixtures/codex/home/` | the usage interval (1 min), incremental | verified (shape, Codex CLI 0.163.0, 2026-11-04 on-prem); token arithmetic **documented** from the Codex source (below) |
| `codex-usage` (opt-in, `quota.codex_usage`) | live quota windows, credits, reset slots and their expiry | two read-only `GET`s to `https://chatgpt.com/backend-api/wham/usage` and `.../wham/rate-limit-reset-credits` with `Authorization: Bearer <access_token>` from `<CODEX_HOME>/auth.json` (`tokens.access_token`, `tokens.account_id` as `ChatGPT-Account-Id`). Usage: `rate_limit.{primary_window,secondary_window}.{used_percent, reset_at, limit_window_seconds}`, `code_review_rate_limit`, `rate_limits_by_limit_id`, `additional_rate_limits[]`, `credits`. Reset slots: `available_count`, `credits[].{status, expires_at}` | default every 3 min, floor 1 min; 429 backoff default 10 min, floor 5 min | **unverified against a real response**: URLs and field names are from decolua/9router v0.5.109 (`open-sse/services/usage/codex.js`, MIT), a third-party client |
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
  A file that started at its end and has no model yet takes, when it next grows, the model of its last `turn_context`
  before that start, searched backwards at most 16 MiB (else `unknown` until its next `turn_context`).
- **First collect cost.** The first collect of a profile reads the tails below (up to 20 MiB) and took 2 to 3.8 s per
  profile on real nodes (steady state 3 to 21 ms); `--once` on several Codex profiles takes that long per profile.
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
- **Reading freshness**, per scope: age must not exceed `max(defaults.stale_after, 2*P + D + 15s)`
  (`P` effective poll interval, `D` collection timeout, default 10s); reset can expire a window earlier. `agent_quota_observed_timestamp_seconds`
  has no `scope` label: it is one series per profile, the newest reading time among the scopes still served, and is
  absent when none is.
- **Credits**, per scope: `agent_quota_credits_available` = `has_credits || unlimited`; `agent_quota_credits_balance` =
  `balance` (a string in Codex, a number accepted) when not unlimited and numeric. A reading with null `credits` keeps
  the scope's previous credits; credits are served at most until the longest window of their own reading resets
  (reading + 7 days with none).
- The existing metric parser does not decode other line types. Event capture may retain locally verified
  structural metadata (including session_meta IDs/cwd/branch/version), never instructions or message
  text; unverified or absent tags remain null. Full historical/archive capture is separate from the
  metric startup-tail optimization. A relevant line over
  4 MiB is skipped and counted as a parse error; an irrelevant line is never buffered.
- Missing or linked `sessions/` is `not_found` / `permission`, never zero values. Links at any level are refused.
- Not exported: session counts (no live-session signal with a pid), Codex `error` events (no real sample of their shape yet).

Rules specific to `codex-usage`:

- **Opt-in per profile** (`quota.codex_usage: true`), off by default. Rollouts keeps its token and request counters always. Its quota
  and credit series are served until `codex-usage` has a reading, and again if that reading exceeds the shared freshness bound and
  the rollouts reading is newer; while `codex-usage` has a fresh reading only it emits them, so no series is emitted twice
  and a failing first read leaves no gap; `codex-usage` emits
  the same `agent_quota_*` series with the same labels, window mapping (`limit_window_seconds` / 60 → the `window_minutes`
  rules above; with none given, the primary window is `5h` and the secondary `7d`) and scope mapping (`rate_limit` is
  `all`; `code_review_rate_limit` is `code_review`; the keys of `rate_limits_by_limit_id` and the ids of
  `additional_rate_limits[]` pass through `scopeLabel`; at most 8 scopes). A window without `used_percent` is absent.
- **The observed time is the read time**, so `time() - agent_quota_observed_timestamp_seconds` is the real age of the quota.
- **The token is never refreshed, logged or written.** OpenAI rotates refresh tokens, so a refresh by the exporter could log
  the real Codex CLI out. `auth.json` is opened read-only without following links, only `tokens.access_token` and
  `tokens.account_id` are decoded, the buffer is zeroed after decoding, and the token lives for one request: never in a
  log line, error, metric or argv. A source error carries only its contract reason. A direct connection is used (no
  environment proxy).
- **Expired token.** If the JWT `exp` of the access token has passed, no call is made; the last reading keeps being served
  with its true observed time (until the shared freshness bound) and `agent_source_errors_total{reason="auth_expired"}` is counted on
  each attempt. Two consecutive 401s stop calls until `auth.json` changes; the reset-slot read has its own such breaker, so its 401s never stop the usage read. A token without a readable `exp` is tried.
- **429** gives `rate_limited` with `sources.codex-usage.backoff_429` (default 10 min, floor 5 min) in the source's own gate
  group (`chatgpt`), so it never pauses Claude or agy and the reverse. Other failures follow the source interval. Spacing
  inside the group is 5 s, so many accounts are all read within one interval.
- **Reset slots** come from the second `GET` of the same read; its failure never drops the quota values just read (it counts
  in `agent_source_errors_total`; a 429 on it also backs the source off). `agent_quota_reset_credits_available` =
  `available_count` (else the number of listed slots whose `status` is not consumed/redeemed/used/expired/revoked/cancelled);
  `agent_quota_reset_credits_next_expiry_timestamp_seconds` = the earliest future `expires_at` over those slots, **absent**
  when none carries one. Both are judged against the current time when served (after a failure or a restart too): a slot past
  its `expires_at` leaves the count and the expiry moves to the next live slot. The path that spends a slot (`.../consume`) is never called, and a test pins the source's URLs.

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
| `opencode-http` | busy sessions of a running server | `GET <url>/session/status` on the profile's `url` (loopback unless `allow_remote: true`; no key; no redirects; 1 MiB and 5 s caps). The answer maps session ids to a status object and lists busy sessions only (`{}` = none); only the **count** of entries whose `type` is not `idle` is kept (`busy`, `retry` and an unknown type count as active), ids and every other field are dropped at the parser. Emits `agent_sessions{state="active"}` only: idle sessions are not listed by the server, so an idle count is unknown and is not emitted. Any failed or malformed read drops the series (source down) | each scrape (default 15 s, floor 5 s) | built; shape from the server's own API, verified on a running server |

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
(`access_token`, `refresh_token`, `value`; the one narrow exception is the `zai` harness's key reader below, which reads
five scalar expressions of `credential` and never the whole `value` column), `permission`, `part` (message text), and `/provider` response keys
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
| `agy-usage` | quota windows | spawn the official client with the fixed argv `agy -p /usage --output-format json`; read `command.data.groups[].buckets[]`: `id`, `window`, `remaining_fraction`, `reset_time`. See `fixtures/agy/usage-command.json` | default every 30 min, floor 2 min (a read costs 0 tokens; the deployment sets `min_interval`); 60 min after a non-zero exit or a timeout; 24 h after the guard below trips | verified (field names, 0 tokens), agy CLI 1.5.0 |

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
  so a backoff on Claude's endpoint never pauses agy and the reverse. Spacing inside the group is 10 s, so many accounts are all read within one interval. `sources.agy-usage.backoff_429` sets the backoff after
  a genuine rate limit (floor 60 min), not every non-zero exit or timeout under the parity requirement.
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
- **Staleness:** the contract's shared freshness bound applies. The last good reading is persisted and served
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
- **`provider`** = the row's (or connection's) provider lowercased if it matches `^[a-z0-9._-]{1,48}$`; empty is
  `unknown`. A label-safe id of 49 to 256 characters (9router's custom providers are `<kind>-compatible-<uuid>`) is
  its prefix without a trailing uuid, cut to 39 characters, plus `-` and the first 8 hex digits of sha256(id), so two
  custom providers keep separate series; anything else is `other`. **`model`** is lowercased and bounded as for `claude-code`, capped per profile across all
  connections at `metrics.max_models_per_profile`; a model past the cap counts into `model="other"`, and one unused for
  a day gives its slot to a new one.
- **Operator enable:** `agent_router_connection_enabled` = 1 if `isActive` is 1, else 0, independently of test status or
  rate limit. Disabled connections retain their existing up=0 and usage counters; consumers can filter failures to
  enabled connections. The setting is persisted with health; older cached state omits this new gauge until a
  successful database read. The existing 50-connection health cap applies.
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

## z.ai (harness `zai`, provider `zai`)

The GLM Coding Plan quota of one z.ai account. z.ai is a vendor reached through several clients, so it has its own
harness and each profile says which local harness login holds the key. **The exporter never asks for a key**: it reads
the one the holder already keeps, read-only, for one request, and nothing else.

| Source | What | How | Cadence | Evidence |
|---|---|---|---|---|
| `zai-usage` | quota windows, plan level, monthly tool-call counts | one `GET https://api.z.ai/api/monitor/usage/quota/limit` per account, header `Authorization: <key>` (no `Bearer`) | default 10 min, floor 5 min; 429 backoff default 30 min, floor 10 min; own network group `zai`, 5 s spacing | endpoint and header **verified** live 2026-11-10 (HTTP 200, level `lite`, three limits); the endpoint is undocumented and community-used |

Opt-in: a profile needs `harness: zai`, `alias`, `account` (the `account` label; never derived from the key), `key_source`,
`dir` and `quota: {zai_usage: true}`. It is never discovered. Inline keys are refused by the config secret scan and by the
unknown-field check.

Key sources (`dir` is the holder's directory). All opens go through `internal/safefs` (no link at any path component,
regular files only, 1 MiB cap); the key must be printable ASCII without whitespace, 16 to 512 bytes; buffers are zeroed
after use.

| `key_source` | `dir` | File and field | Evidence |
|---|---|---|---|
| `openclaw` | `~/.openclaw` (OpenClaw state dir) | `<dir>/openclaw.json`, JSON5 (comments, trailing commas and a BOM are understood; single quotes and unquoted keys are not). First present of `models.providers.zai.apiKey`, `models.providers.zai-anthropic.apiKey`, then `env.vars.ZAI_API_KEY`; a non-string (SecretRef object) is `parse` | structure **verified** by key names on a real file 2026-11-10 and the key works on the endpoint; the `env.vars` fallback is **documented** (OpenClaw `docs/providers/zai.md`), not seen live |
| `opencode` | the opencode data dir (`~/.local/share/opencode`) | (a) legacy `<dir>/auth.json`: `{"<integrationID>": {"type":"api","key":"..."}}`; else (b) `<dir>/opencode.db` table `credential`, `$.key` where `$.type == "key"`; integration ids in order `zai-coding-plan`, `zhipuai-coding-plan`, `zai`, `zhipuai`; per id the `active` row, else the newest `time_updated`. The query is `ByKeys` on those four ids selecting `integration_id`, `active`, `time_updated`, `$.type`, `$.key` only | **verified** from opencode's source (credential import migration and `credential` table definition) and the local `opencode.db` schema (1.18.33, no rows on that node). **Not verified**: a real z.ai row |
| `zai-helper` | `~/.chelper` | `<dir>/config.yaml`, field `api_key` | **verified** from the package source (`@z_ai/coding-helper` 0.1.1). The helper also copies the key into `~/.config/opencode/opencode.json` and `~/.codex/config.toml`; the exporter does not read those |
| `zcode` | n/a | **unsupported, rejected at config load.** zcode's login is an OAuth "business token" the runtime validates and decrypts; it is not a plain key and nothing shows `/api/monitor` accepts it | **documented**, unproven |

Rules:

- Read-only toward the holder, never refresh, never log in, never call another z.ai endpoint. Only `https://api.z.ai`
  is ever requested (the China host `open.bigmodel.cn` was not verified for this path and is not shipped; there is no
  `region` or `base_url` setting). No proxy, no keep-alive, redirects refused, 10 s timeout, 1 MiB body cap.
- The key is re-read on every poll, so a rotation is picked up. It never appears in a log, error, metric, label, state
  file or argv.
- **Duplicate keys.** Two profiles whose keys are equal (compared in memory by hash, never stored) are one account:
  the profile with the smallest `alias` reads it, the other makes no request, emits no series and stays `up`, with one
  warning naming the two aliases. A peer whose key cannot be read is ignored.
- **Failures** (reason, behaviour): missing dir, file or entry `not_found`; a link or non-regular file `permission`;
  malformed file, non-string key, bad key shape, over 1 MiB or an unexpected envelope `parse`; HTTP 401/403, or HTTP 200
  with `success:false` and `code` 1000 to 1004, `auth_expired` (verified live: a fake key gives 200 with code 1000, no
  header gives 200 with code 1001); HTTP 429, or 200 with `success:false` and code 1302 or 1305, `rate_limited` with the
  backoff above (1302/1305 are **documented**, not seen); other `success:false`, other 4xx/5xx, TLS/DNS failures,
  redirects and oversize bodies `upstream`; a deadline `timeout`. HTTP 200 alone never means data. After two
  `auth_expired` in a row with the same holder file revision, calls stop until that file changes. The last good reading
  keeps being served with its true observed time until the shared freshness limit.
- **Mapping** (closed): `TOKENS_LIMIT` unit 3 number n is window `<n>h`, scope `all`; `TOKENS_LIMIT` unit 6 number n is
  `<7n>d`, scope `all`; `TIME_LIMIT` unit 5 number 1 is `monthly`, scope `tool_calls`. The unit codes are **inferred**
  from live responses (unit 3 number 5 is the 5-hour window; unit 6 number 1 resets a week out; unit 5 number 1 is the
  monthly allowance). Anything else is not exported and counted once in `agent_source_errors_total{reason="parse"}`;
  the read still succeeds. Two limits mapping to the same labels merge (highest ratio, earliest reset).
- **Values.** `agent_quota_utilization_ratio` is `percentage / 100`; a `TIME_LIMIT` without `percentage` uses
  `currentValue / usage` when `usage > 0`; otherwise the series is absent, never 0. `nextResetTime` is milliseconds
  (kept only when plausible); a window not started has none. `agent_quota_used_calls` and `agent_quota_limit_calls` are
  `currentValue` and `usage` of the `TIME_LIMIT` window only (z.ai names the cap `usage`). `agent_account_info{plan}`
  is `data.level` lowercased (`lite`, `pro`, `max`, `team`, else `unknown`).
- Not built: per-tool `usageDetails` series, `model-usage` (returns no data), credits or money (D9), events for this
  source, discovery of z.ai profiles.

## Jobs (harness `jobs`, source `job-locks`; host-level)

Opt-in with `jobs.lock_dir` (absolute path; empty = off). One source per host, not per profile: a task runner can hold
a lock for an account that has no exporter profile at all, and the source must still see it.

The convention it reads is a plain one: while a job for account `A` of tool `T` runs, the runner holds an exclusive
`flock` on the empty file `<lock_dir>/T-A.lock`, and appends one JSON line per call to a call log (`jobs.call_log`,
optional). Only names matching `^(codex|agy)-[A-Za-z0-9][A-Za-z0-9_.-]{0,63}\.lock$` are listed, regular files only
(no link followed), at most 100 per cycle (the rest add to `agent_source_errors_total{reason="other"}`).

| Source | What | How | Cadence | Evidence |
|---|---|---|---|---|
| `job-locks` | which accounts have a job running now; end time and exit code of the last finished one | **running**: the file's `major:minor:inode` (from a no-follow `lstat`, nothing opened) matches a `FLOCK` row of `/proc/locks` that is not a waiter line (`->`); the flock itself is never taken, so the source cannot make a runner wait or fail. **last job**: the call log is read by byte offset (kept in the exporter's state; first run reads at most the last 8 MiB; a log shorter than the offset was replaced and is not replayed; an unfinished last line waits for the next cycle; a line over 64 KiB or not JSON counts as `parse`). Only `ts`, `duration_s`, `exit_code`, `tool` and `account` are decoded: end = `ts` (call start) + `duration_s`. A row counts only if `tool` is `codex` or `agy`, `account` matches the name rule above and has a lock file in the directory, and `exit_code` is not 75 or 124 (a call that found the account busy, or gave up waiting, ran no job). The latest end per account wins. Caller, directory, session id, task hash and task length are not decoded | each scrape (default 15 s, floor 5 s) | built; the `/proc/locks` row format was checked against the real kernel table (hex `major:minor`, decimal inode, waiter lines) |

Limits, by design:

- The lock is a stand-in for "a job runs": if the runner process is killed outright (SIGKILL) while its child keeps
  working, the lock is gone and the job shows as not running.
- A call waiting for the lock (polling a busy account) is invisible; only the job that holds the lock shows.
- Direct use of an agent CLI that does not go through the runner takes no lock and writes no log line: invisible.
- `/proc/locks` is Linux-only; elsewhere the source reports itself down.
- Account names are labels; the `profile` and `account` labels both carry the account part of the file name, and
  `harness` the tool prefix.
- If `/proc/locks` or the lock directory cannot be read, the series disappear and the source is down; nothing is
  guessed.

## Other providers (later, by plugin)

| Provider | Official surface | Scope |
|---|---|---|
| Anthropic API keys (billing) | Admin usage and cost reports API | in scope as an optional provider plugin; hourly-bucket data, daily-lagged; needs an admin key (a secret the operator provides via env or file reference) |
| OpenAI API keys (billing) | organization usage/costs API | same pattern |
| OpenRouter, DeepSeek and similar | balance endpoints with an API key | not built (`specs/decisions.md` D9: prices are not known in real time) |
| z.ai GLM Coding Plan | quota windows of the plan, with a key a local harness already holds | built as `zai-usage` (opt-in), see the z.ai section; quota windows only, no balance, D9 unaffected |
| Google AI subscription windows | the agy client's own `/usage` command | in scope as `agy-usage` (opt-in), see the agy section; nothing calls the private endpoint itself |

Keys for these plugins are referenced from config as `env:NAME` or `file:/path` and are never inlined in the config
file.

## Out of scope

Parity failure policy: ordinary timeout/network/5xx/parse failures preserve last-good observations,
append classified attempts and retry at the normal interval; repeated local/agy failures use 2x/4x
backoff capped at 15 min. Actual agy 429 remains >=1 h; zero-cost/guard failure remains 24 h.
Missing local files retry normally. Direct Claude remains >=900 s/account globally, 15 s/no inline
retry, >=3600 s account-wide cooldown on 429; repeated non-rate failures may extend to one hour.
Persist gates across restart; credential changes never clear rate limits. A generic external permit
supports coordination without fleet code. Corrupt network gate state keeps one-hour quarantine.
Active Claude uses the shared freshness bound (effective poll interval at least 900 s), or reset-passed; retained history remains readable.

The agy table and refresh paragraphs document existing behavior, which is **not** proof of compliance:
its unconditional ordinary-failure one-hour suppression is superseded by the policy above. Its CLI
can refresh credentials, conflicting with the parity no-client-launch/no-refresh boundary. The brief
also requires retaining the verified agy reader; this conflict needs an explicit integration decision
before runtime parity can pass. B01 neither activates a new endpoint nor authorizes credential mutation.

Manual Mirai receipts are an import contract, not a new exporter provider plugin: preserve summary
and paginated/since request facts with units, original manual age and unknown tags per event-contract.
Unverified monetary/non-monetary charge units remain restricted evidence with unknown presentation.

- Anything that requires **refreshing, minting or rotating an OAuth token**, or logging a client in or out.
- Calling a model (no inference to "test" an account; no tokens spent by the exporter).
- Reading any prompt, completion, tool input/output or file content from any agent's state.
- Reading any credential file beyond the one value or timestamp a declared plugin needs.
- Router and gateway internals other than 9router (see its section). Each would be its own plugin, always behind a
  column allow-list, since their tables hold provider tokens and API keys.
- Host CPU/memory/disk metrics: use `node_exporter`.
- Writing to or modifying any agent configuration.
