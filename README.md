# agent-quota-exporter

> Status: **v0.2 (pre-release).** Claude Code (v0.1), plus Codex, opencode, a 9router router and agy
> ([plans/261104-1800-more-sources](plans/261104-1800-more-sources/plan.md)). Provider balance endpoints, OTLP and
> packaging are not built yet; see [specs/plan.md](specs/plan.md#phases-and-acceptance-criteria).

A read-only Prometheus exporter for AI coding agents. It reports, per machine, per workspace and per account:

- **subscription quota windows** (for example a 5-hour and a 7-day window, with utilization and reset time);
- **per-agent usage** (tokens, requests, errors, sessions) read from the agents' own local state.

It works across vendors and harnesses (Claude Code, Codex, opencode, agy and others, one plugin each) and is built
for the case where one machine runs **several accounts in several workspaces**.

## Why

People who run coding agents on subscriptions hit limits they cannot see coming, and teams that run many accounts
cannot tell which account is exhausted, expired or idle. The existing open-source exporters each cover a slice:

- some read local logs for token usage but have no quota windows and no multi-account model;
- some export quota windows, but do it by calling private vendor endpoints with CLI OAuth tokens and may refresh
  those tokens, which can revoke the live CLI session;
- billing exporters cover official API keys and admin APIs, not subscriptions.

No open-source project we found combines **subscription quota windows + per-agent usage, across vendors,
multi-account, without ever touching a token**. That is the gap. (Prior art and how it is used: see
[specs/plan.md](specs/plan.md#prior-art). Ideas are borrowed; no code is.)

## Principles

1. **Vendor-agnostic and fleet-agnostic.** The exporter knows nothing about any user's inventory, graph, host names or
   naming scheme. Output labels are generic: `host`, `harness`, `profile`, `account`, `provider`, `model`, `window`.
   Mapping those labels to anything else (a CMDB, a topology graph) is the consumer's job, done downstream.
2. **Multiple accounts in multiple workspaces on one machine is the norm.** A *profile* is one workspace: a Claude
   `CLAUDE_CONFIG_DIR`, a Codex `CODEX_HOME`, one opencode instance and its data dir, one agy `HOME`. One OAuth
   grant per profile, never copied between profiles.
3. **Read-only. Never refreshes or writes a token.** Refresh tokens are single-use: a second refresher (this
   exporter) would invalidate the grant the real client holds and log the user out. If an access token has expired,
   the exporter reports the profile as stale and waits for the real client to refresh it.
4. **Rate limits are per account, not per scrape.** Vendor usage endpoints lock out pollers. Every source has a
   minimum interval and a cache; Prometheus can scrape every 15 seconds and the vendor is still called at most once
   per interval per account, with backoff after a 429.
5. **Secrets never leave the file.** Tokens are read only inside the one plugin that needs them, held in memory for
   one request, and never logged, printed, put in an error message or a label. Credential files are opened only
   by plugins that declare it; everything else is allow-listed by column or key (see [specs/sources.md](specs/sources.md)).
6. **No message content, ever.** Transcripts, prompts and tool output are parsed for structural fields only
   (timestamps, model, usage numbers). Content fields are dropped at the parser.
7. **Prometheus first, OTLP optional.** `/metrics` is the contract ([specs/metric-contract.md](specs/metric-contract.md)).
   Cardinality is bounded by design: no session or request ids on hot series.
8. **Own config file, explicit workspaces, plus auto-discovery** of the standard locations.

## Safety notes

- The exporter needs read access to agent state directories and, for quota plugins only, to the credential file of
  each profile. Run it as the same user that owns the profiles, or give it a read-only group on those directories.
  It needs no write access to any of them (its own cache and offsets live in its own state dir).
- Opening a local SQLite database is done with `mode=ro` (read-only, `immutable` is not used on live databases).
  Never take a lock that could block the agent that owns the database.
- Quota plugins that call a vendor endpoint are **opt-in per profile** and flagged in the docs when the endpoint is
  private or undocumented. Anything that would need a token refresh is out of scope (or opt-in and marked risky).
- Fixtures in this repository are synthetic. Nothing in this repository is real telemetry.

## Usage

```
agent-quota-exporter --config /etc/agent-quota-exporter/config.yaml   # serve /metrics and /healthz (127.0.0.1:9877)
agent-quota-exporter --config FILE --once                              # collect local sources once, print, exit
agent-quota-exporter --config FILE --once --once-with-quota            # also ask the quota endpoint if its gate allows
agent-quota-exporter --config FILE --check-config                      # load and validate the config, exit 0 or 1; listens on nothing, writes nothing
agent-quota-exporter --version
```

- Config: [examples/config.yaml](examples/config.yaml). With no config file, profiles are auto-discovered under
  `/root` and `/home/*`: `.claude` and `.claude-*` dirs, `.codex` (with `sessions/`) and `.local/share/opencode`
  (with `opencode.db`). Router and agy profiles exist only when declared. Sources:

  | Source | Harness | Default | Reads |
  |---|---|---|---|
  | `claude-transcripts`, `claude-sessions`, `claude-account` | `claude-code` | on | local state |
  | `claude-statusline-cache` | `claude-code` | on for `/root/.claude` only | AgentKit status line cache, no credentials or vendor call |
  | `claude-oauth-usage` | `claude-code` | opt-in, `quota: {claude_oauth_usage: true}` | the profile's token and Claude's quota endpoint |
  | `codex-rollouts` | `codex` | on | quota, credits and tokens the Codex CLI wrote to its rollouts |
  | `codex-usage` | `codex` | opt-in, `quota: {codex_usage: true}` | live Codex quota windows, credits and reset slots with their expiry, from the ChatGPT backend with the CLI's own access token; takes over the quota series of `codex-rollouts` for that profile while it has a fresh reading (rollouts serves them before the first live read and if the live one goes stale; its token counters always stay) |
  | `opencode-db` | `opencode` | on | tokens, cost and errors from `opencode.db`, read-only |
  | `opencode-http` | `opencode` | opt-in, an opencode profile with `url: http://127.0.0.1:PORT` | busy-session count from the running server's `GET /session/status` |
  | `job-locks` | `jobs` | opt-in, `jobs: {lock_dir, call_log}` | which agent accounts have a job running now, from a task runner's lock files and `/proc/locks` |
  | `router-9router` | `9router` | declared profiles only | requests, tokens and connection health from the router's DB, read-only |
  | `zai-usage` | `zai` | opt-in, a `zai` profile with `quota: {zai_usage: true}` | z.ai GLM Coding Plan quota windows and plan level, with the key a local OpenClaw, opencode or z.ai coding helper login already holds |
  | `agy-usage` | `agy` | opt-in, `quota: {agy_usage: true}` plus `account` | runs the agy client's own `/usage` command; each poll can trigger agy's normal token refresh |

- **What is running now** (all opt-in, read-only, no prompt, task or screen text ever read or exported):
  - `metrics.per_session: true` adds, for each live Claude Code session, `agent_session_state` (busy, idle, waiting,
    shell, other), `agent_session_waiting` (what a waiting session waits on: permission, input, dialog, sandbox,
    worker, goal, other), `agent_session_state_since_timestamp_seconds` and `agent_session_info`. The `session` label
    is the session's name made safe, which in practice is the project folder name plus a short suffix.
    `agent_session_info` also carries a `pid` label (the session's OS process id, empty unless the session file's
    start time and pid domain prove it is a live process on this host), to join a session to a process. At most
    `metrics.max_sessions_per_profile` (default 50) per profile. A session counts as live only if its process is
    the one the file was written for (same start time, same host).
  - `jobs.lock_dir` (and optional `jobs.call_log`) adds `agent_job_running` and the last job's end time and exit
    code per agent account, from a task runner that flocks `<codex|agy>-<account>.lock` while a job runs. The
    exporter never takes that lock. Jobs started without the runner are invisible.
  - An opencode profile with `url` adds the busy-session count of that server (loopback only unless
    `allow_remote: true`).
  - If any of these cannot be read, their series disappear and `agent_source_up` is 0; nothing is guessed. Details
    and limits: [specs/sources.md](specs/sources.md), [specs/metric-contract.md](specs/metric-contract.md).
- `codex-usage` (fresh Codex quota): default every 3 min (floor 1 min), 429 backoff default 10 min (floor 5 min); set
  `sources.codex-usage.min_interval` to change it. It reads only the access token in `<CODEX_HOME>/auth.json`, sends it to
  `chatgpt.com` over a direct connection and never refreshes it (OpenAI rotates refresh tokens, so a refresh here could
  log the real Codex CLI out) and never writes `auth.json`. When the token is expired the call is skipped, the last
  reading keeps being served with its true observed time, and `agent_source_errors_total{reason="auth_expired"}` counts
  each attempt until the CLI refreshes its login by running. It also reads the reset slots (read-only; the
  exporter never spends one): `agent_quota_reset_credits_available` and `agent_quota_reset_credits_next_expiry_timestamp_seconds`.
  The endpoints are private and undocumented by the vendor; the field names come from a third-party client's code.
- `zai-usage` (z.ai GLM Coding Plan quota): **the exporter never asks for a key.** A `zai` profile names a harness login
  that is already on the box (`key_source: openclaw | opencode | zai-helper`) and its directory; the exporter reads the
  key from that holder's own file, read-only, sends it to `api.z.ai` only, and never logs or stores it. Default every
  10 min (floor 5 min), 429 backoff 30 min (floor 10 min). Two profiles holding the same key are read once. Example:

  ```yaml
  - harness: zai
    alias: zai-openclaw
    account: zai-main                 # the `account` label; required
    key_source: openclaw              # openclaw | opencode | zai-helper
    dir: /home/acct-a/.openclaw       # the holder's directory
    quota: { zai_usage: true }
  ```

  Details, evidence levels and failure reasons: [specs/sources.md](specs/sources.md), section "z.ai".
- `agy-usage` interval: default 30 min, floor 2 min. A read runs the client's own `/usage` command (0 tokens), so set
  `sources.agy-usage.min_interval` (for example `3m`) for a live dashboard; it still backs off after a failed run.

  Read-only SQLite sources read a WAL database that no writer has open (no `-wal`/`-shm`) in SQLite's immutable
  mode, which creates nothing; with a writer present they attach to the existing `-shm` like any SQLite reader. A
  `-wal`/`-shm` that is a link, a hard link or not a regular file is refused before SQLite runs.
- Claude status line cache: only the profile whose resolved `dir` is `/root/.claude` reads it, using the same
  account label as that profile's OAuth source. The path is the profile's
  `quota: {claude_statusline_cache: "/tmp/ck-usage-limits-cache.json"}` override, else `CK_USAGE_CACHE_PATH`
  from the exporter's own environment, else `/tmp/ck-usage-limits-cache.json`. It is read-only and polled every
  `defaults.usage_interval` (default 60 s). `status: available` and valid quota data are required; the observation
  time is the file's millisecond epoch `timestamp`. The opened file must be owned by uid 0 and must not be
  group- or other-writable; rejected files report `untrusted_owner`. The timestamp must be at least the profile's
  `.credentials.json` modification time (checked without reading credentials); older readings report `auth_expired`.
   Freshness uses the rule below; the legacy `defaults.statusline_stale_after` setting is accepted but ignored. Missing, unreadable, invalid or unavailable files give no
  reading and mark the source down, without advancing its last-success timestamp. With `claude_oauth_usage: true`
   on the same profile, a valid local reading defers OAuth only while its age is at most five minutes.
   This local-preference policy controls attempts, not freshness: exposition still uses the shared rule below.
   After writes stop, OAuth may poll subject to its existing 15-minute floor, persisted gates and backoff.
   Exposition selects the newer eligible reading (local wins ties). There is no vendor call without that opt-in.
- systemd: [examples/agent-quota-exporter.service](examples/agent-quota-exporter.service). The shipped unit has
  **`PrivateTmp=true`, which hides the host `/tmp` cache**. To use the default cache, override it with
  **`PrivateTmp=false`**, or provide a **read-only bind** of the host cache at a path visible to the service and
  set `quota.claude_statusline_cache` to that path. Bind the containing directory read-only when possible:
  the status line replaces the file atomically, so a bind of one file can retain an old inode. Keep the unit's
  other hardening settings.
- `--once` takes the same state-dir lock as the daemon, so it refuses to run next to a running exporter. Point it
  at a copy of the config with its own `state_dir` for ad-hoc checks; the quota endpoint is still asked at most once
  per 15 min per state dir.
- Releases: pushing tag `vX.Y.Z` (or `vX.Y.Z-rc.N` for a prerelease) builds static linux amd64/arm64 tarballs and
  `SHA256SUMS`; where tags cannot be pushed, dispatch the `release` workflow on `main` with the tag name, or push a
  branch `release/vX.Y.Z[-rc.N]` at the commit to release, and the workflow creates the tag. Final tags must be on
  `main`. Verify with `sha256sum -c SHA256SUMS`.
- Develop: `go vet ./... && go test -race ./...`. The golden output is `testdata/golden/metrics.txt`; after an
  intended change, review the diff and run `go test ./internal/app -update`.

### Reading freshness

Every source's reading is stale when its age **exceeds** `max(defaults.stale_after, 2*P + D + 15s)`.
`P` is its effective poll interval (`MinInterval()`); `D` is its collection timeout (`ReadTimeout()`,
or the core default of 10s). Exactly at the limit it is fresh. `stale_after` defaults to 45m and accepts
any positive duration, including 5m. The legacy statusline-specific setting is ignored.
Failed polls, backoff and scrapes never renew observation time. Stale values are absent, never zero;
quota resets can make a value ineligible earlier. Passive readings use their original event time.
Historical evidence remains readable. Polling intervals and vendor rate gates are independent of freshness.

## Layout

| Path | What |
|---|---|
| `specs/plan.md` | architecture, config, discovery, caching, errors, packaging, phases, Go vs Python recommendation |
| `specs/metric-contract.md` | metric names, types, units, labels, cardinality rules, examples |
| `specs/sources.md` | per harness and provider: what is read, how, cadence, secret hazards, out of scope |
| `specs/decisions.md` | open and settled decisions (licence locked; language, public timing, per-session open) |
| `fixtures/` | sanitized sample inputs for offline development (see `fixtures/README.md`) |
| `AGENTS.md` | rules for AI agents working in this repo (`CLAUDE.md` is a symlink to it) |
| `.aiworkflow/` | work ledger and component manifest |
| `cmd/`, `internal/` | the exporter: `config` (load, validate, discover), `core` (scheduler, state, exposition), one package per harness (`claude`, `codex`, `opencode`, `router`, `agy`), `safefs` and `sqlro` (no-link files, allow-listed read-only SQLite), `app` (wiring and end-to-end test) |
| `examples/` | example config and systemd unit |
| `plans/` | build plans with their red-team review |

## Licence

**Apache-2.0** (see `LICENSE`), the licence of node_exporter and the other prometheus/ exporters. Locked 2026-10-28; see
[specs/decisions.md](specs/decisions.md).
