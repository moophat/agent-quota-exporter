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
- Never write to any agent directory. Never invoke an agent CLI that can start sessions or refresh logins, unless the
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
| `codex-rate-limits` | primary and secondary window `usedPercent`, `resetsAt`, reset credits | JSON-RPC over stdio to `codex app-server`: `account/read`, `account/rateLimits/read` (shape in `fixtures/codex/rate-limits-response.json`, synthetic) | at most once per account per 15 min | documented, **unverified** (the node used for fixtures is not logged in) |
| `codex-state` | sessions, usage | `<CODEX_HOME>/state_*.sqlite` (and `logs_*.sqlite`): schema not yet inspected; read-only, explicit columns | 1 to 5 min | **unverified**, schema discovery is a phase task |
| `codex-auth` (presence only) | logged-in state | check that `<CODEX_HOME>/auth.json` exists and its mtime; do not read its contents | each scrape | verified (name) |

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
| `opencode-db` | per-message tokens, cost, model, provider, error | `opencode.db` SQLite, read-only. `message.data` is JSON with `role`, `providerID`, `modelID`, `cost`, `tokens{total,input,output,reasoning,cache{read,write}}`, `finish`, `time`; pull these with `json_extract` so no other part of the JSON leaves SQLite. Incremental by `message.time_created` / rowid. Session roll-ups: `session.tokens_*`, `session.cost`, `session.model`, `session.agent`. Schema in `fixtures/opencode/opencode.schema.sql` | 1 to 5 min | verified |
| `opencode-http` | live sessions, connected providers | `GET /session` on the instance (tokens, cost, model per session); `GET /provider` for connected providers: **read only the provider ids and the `connected` list** | each scrape, cached 30 s | documented, partly verified |

Known data-quality traps (from observed instances):

- A proxy-fronted lane can under-report: a large share of its calls show zero usage. Export what is stored and let the
  consumer judge; do not "fix" it.
- Cost can be `0` for subscription providers. `agent_cost_usd_total` is only exported when a provider reports a non-zero
  cost at least once.
- Provider ids stored in historical rows can differ from the live config after a rename; labels follow the stored value.

Never-read (hard blocklist, enforced by the query allow-list): tables `account`, `control_account`, `credential`
(`access_token`, `refresh_token`, `value`), `permission`, `part` (message text), and `/provider` response keys
`key` and `options`.

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
- **Token refresh is the client's.** A run with an expired access token refreshes it and rewrites the token file, as any
  agy run does; two runs with a live token changed nothing (on-prem, by hash and mtime only). So each poll can trigger the
  client's normal refresh. Whether the refresh token itself rotates is re-checked on-prem on an rc before `v0.2.0`; if it
  rotates, the source stays out of the release.
- **Timeout:** 30 s per run. The client runs in its own process group; on timeout the group gets `SIGTERM`, then 5 s to
  finish writing its token file, then `SIGKILL`. Never `SIGKILL` first.
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

## Other providers (later, by plugin)

| Provider | Official surface | Scope |
|---|---|---|
| Anthropic API keys (billing) | Admin usage and cost reports API | in scope as an optional provider plugin; hourly-bucket data, daily-lagged; needs an admin key (a secret the operator provides via env or file reference) |
| OpenAI API keys (billing) | organization usage/costs API | same pattern |
| OpenRouter, DeepSeek, GLM and similar | balance endpoints with an API key | optional plugin, GET + JSON path pattern, multiple accounts via numbered key references |
| Google AI subscription windows | none known without a private endpoint | out of scope |

Keys for these plugins are referenced from config as `env:NAME` or `file:/path` and are never inlined in the config
file.

## Out of scope

- Anything that requires **refreshing, minting or rotating an OAuth token**, or logging a client in or out.
- Calling a model (no inference to "test" an account; no tokens spent by the exporter).
- Reading any prompt, completion, tool input/output or file content from any agent's state.
- Reading any credential file beyond the one value or timestamp a declared plugin needs.
- Router and gateway internals (per-account health inside a proxy such as a model router) are a separate class of
  source: possible later as plugins, always with a column allow-list (their tables hold provider tokens and API keys).
- Host CPU/memory/disk metrics: use `node_exporter`.
- Writing to or modifying any agent configuration.
