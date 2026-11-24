# agent-quota-exporter

A read-only Prometheus exporter for AI coding agents. On each machine it reports, per workspace and per account:

- **subscription quota windows**: for example a 5-hour and a 7-day window, with utilization and reset time;
- **per-agent usage**: tokens, requests, errors, cost as the harness reports it, and live sessions, read from the
  agents' own local state;
- **health of every source**, so a missing value is visibly unknown, never a silent zero.

It works across vendors and harnesses, one plugin each, and is built for the case where one machine runs **several
accounts in several workspaces**. It never refreshes, writes or exports a token.

> Status: **pre-release** (v0.3 release candidates; metric contract version 0). Metric names may still change before
> 1.0; every change is listed in [CHANGELOG.md](CHANGELOG.md).

## Contents

- [Why](#why)
- [Supported harnesses and sources](#supported-harnesses-and-sources)
- [Install](#install)
- [Quick start](#quick-start)
- [Command line](#command-line)
- [Configuration reference](#configuration-reference)
- [Metrics](#metrics)
- [Security and privacy](#security-and-privacy)
- [Running under systemd](#running-under-systemd)
- [Contributing and repository layout](#contributing) (`fixtures/` and `testdata/` hold synthetic data only)
- [Security reports](#security-reports), [License](#license)

## Why

People who run coding agents on subscriptions hit limits they cannot see coming, and teams that run many accounts
cannot tell which account is exhausted, expired or idle. Existing open-source exporters each cover a slice: some read
local logs for tokens but have no quota windows and no multi-account model; some export quota windows but call
private vendor endpoints with CLI OAuth tokens and may refresh those tokens, which can log the real CLI out; billing
exporters cover API keys, not subscriptions.

This exporter combines subscription quota windows and per-agent usage, across vendors and accounts, without ever
refreshing a token. Design principles:

1. **Vendor-agnostic and deployment-agnostic.** Output labels are generic (`host`, `harness`, `profile`, `account`,
   `provider`, `model`, `window`). Mapping them to your inventory is done downstream.
2. **Several accounts per machine is the norm.** A *profile* is one workspace: a Claude `CLAUDE_CONFIG_DIR`, a Codex
   `CODEX_HOME`, an opencode data dir, an agy `HOME`.
3. **Read-only.** Never refreshes or writes a token. Refresh tokens are single-use: a second refresher would
   invalidate the grant the real client holds. An expired token makes the profile stale until the client refreshes it.
4. **Rate limits are per account, not per scrape.** Every vendor call has a minimum interval, a persisted gate and
   backoff. Prometheus may scrape every 15 s; the vendor is still called at most once per interval per account.
5. **No message content, ever.** Transcripts are parsed for structural fields only (timestamps, model, token counts).
6. **Bounded cardinality.** No session or request ids on default series; model labels are capped per profile.

## Supported harnesses and sources

A *harness* is the agent program or service a profile belongs to. Each harness has one or more *sources*. Sources
marked **opt-in** do nothing until the config switches them on.

| Harness (`harness` label) | What it is | Source | Default | Reads |
|---|---|---|---|---|
| `claude-code` | Anthropic's Claude Code CLI | `claude-transcripts` | on | token usage from `<config>/projects/**/*.jsonl`, structural fields only |
| | | `claude-sessions` | on | live session count from `<config>/sessions/*.json`; per-session state with `metrics.per_session` |
| | | `claude-account` | on | account id, plan and credential expiry time (never the token) |
| | | `claude-oauth-usage` | **opt-in** `quota.claude_oauth_usage: true` | quota windows from Anthropic's usage endpoint, with the profile's own access token; at most once per 15 min per account |
| | | `claude-statusline-cache` | **opt-in** `quota.claude_statusline_cache: /abs/path` | quota windows from a cache file your status line script writes; no credentials, no vendor call ([format](#claude-status-line-cache)) |
| `codex` | OpenAI's Codex CLI | `codex-rollouts` | on | quota windows, credits and tokens the CLI wrote to its rollout files |
| | | `codex-usage` | **opt-in** `quota.codex_usage: true` | live quota windows, credits and reset slots from the ChatGPT backend with the CLI's own access token |
| `opencode` | the opencode CLI and server | `opencode-db` | on | tokens, cost and errors from `opencode.db`, read-only |
| | | `opencode-http` | **opt-in** profile `url` | busy-session count from a running server's `GET /session/status` |
| `agy` | the agy agent CLI (provider `google`) | `agy-usage` | **opt-in** `quota.agy_usage: true` | quota buckets by running the client's own `/usage` command (0 tokens) |
| `9router` | the [9router](https://github.com/decolua/9router) local AI router | `router-9router` | declared profiles only | requests, tokens and upstream connection health from the router's SQLite database, read-only |
| `zai` | z.ai GLM Coding Plan | `zai-usage` | **opt-in** `quota.zai_usage: true` | quota windows and plan level, with a key that a local OpenClaw, opencode or z.ai coding-helper login already holds |
| `strata` | an HTTP model server exposing JSON `/slots` and `/metrics` | `strata` | **opt-in** top-level `strata:` | busy, queued and slot counts per server ("lane") |
| `jobs` | a task runner that holds one flocked `<tool>-<account>.lock` per running job (tools `codex`, `agy`) and appends a JSONL call log | `job-locks` | **opt-in** `jobs.lock_dir` | which accounts have a job running now (Linux `/proc/locks`) |

The per-source details (exact files, fields, endpoints, cadence, evidence level and failure reasons) are in
[specs/sources.md](specs/sources.md).

**Auto-discovery.** Unless `discovery.mode: off`, the exporter looks one level under each `discovery.roots` entry
(default `/root` and `/home/*`) for `.claude` and `.claude-*` dirs, `.codex` (with `sessions/`) and
`.local/share/opencode` (with `opencode.db`), plus `$CLAUDE_CONFIG_DIR` and `$CODEX_HOME` from the exporter's own
environment. Discovered dirs that are symbolic links are skipped. agy, 9router and zai profiles exist only when
declared.

## Install

Pre-built static binaries for Linux amd64 and arm64 are attached to each
[GitHub release](https://github.com/moophat/agent-quota-exporter/releases), with a `SHA256SUMS` file:

```sh
VERSION=v0.3.0-rc.13      # pick a release from the releases page
ARCH=amd64                # or arm64
curl -LO "https://github.com/moophat/agent-quota-exporter/releases/download/${VERSION}/agent-quota-exporter_${VERSION}_linux_${ARCH}.tar.gz"
curl -LO "https://github.com/moophat/agent-quota-exporter/releases/download/${VERSION}/SHA256SUMS"
sha256sum -c --ignore-missing SHA256SUMS
tar -xzf "agent-quota-exporter_${VERSION}_linux_${ARCH}.tar.gz"
sudo install -m 0755 "agent-quota-exporter_${VERSION}_linux_${ARCH}/agent-quota-exporter" /usr/local/bin/
```

Each tarball also holds `LICENSE`, this README, `config.yaml` and the systemd unit.

From source (Go 1.24 or newer):

```sh
go install github.com/moophat/agent-quota-exporter/cmd/agent-quota-exporter@main    # or @vX.Y.Z for a tag
```

Use `@main` or an explicit tag: until 0.3.0 is final, `@latest` resolves to the older v0.1.0. A `go install` build
reports its version as `dev`; release builds set it with `-ldflags "-X main.version=..."`.

## Quick start

1. Write a config (or start with none and rely on auto-discovery):

   ```yaml
   # /etc/agent-quota-exporter/config.yaml
   version: 1
   profiles:
     - harness: claude-code
       alias: work
       dir: /home/alice/.claude
   ```

2. Validate it: `agent-quota-exporter --config /etc/agent-quota-exporter/config.yaml --check-config` prints
   `config ok` or every problem found, and exits 0 or 1.
3. Look at one collection without starting a server:
   `agent-quota-exporter --config /etc/agent-quota-exporter/config.yaml --once`. It prints the metrics text to stdout,
   for example `agent_tokens_total{...,type="output"} 1234` and `agent_source_up{...} 1` lines.
4. Run it: `agent-quota-exporter --config /etc/agent-quota-exporter/config.yaml`, or install the
   [systemd unit](#running-under-systemd). It serves `/metrics` and `/healthz` on `127.0.0.1:9877`.
5. Scrape it from Prometheus:

   ```yaml
   scrape_configs:
     - job_name: agent-quota-exporter
       scrape_interval: 30s
       static_configs:
         - targets: ["127.0.0.1:9877"]
   ```

Example alert expressions:

```promql
agent_quota_utilization_ratio > 0.9                                   # a quota window is almost used up
agent_source_up == 0                                                  # a source cannot be read
(agent_source_up{source=~"claude-oauth-usage|claude-statusline-cache|codex-usage|codex-rollouts|agy-usage|zai-usage"} >= 0) unless on(host, harness, profile) agent_quota_observed_timestamp_seconds  # a configured quota source has no fresh reading
agent_account_logged_in == 0                                          # a profile has no usable login
```

Because `agent_quota_observed_timestamp_seconds` is omitted once a reading exceeds the freshness cutoff (default `45m`),
an age check alone (`time() - agent_quota_observed_timestamp_seconds > 3600`) loses its input series once stale; matching
against the persistent `agent_source_up` series with `unless` detects any configured quota source that currently lacks a
fresh quota sample (whether past the freshness cutoff or never observed).

## Command line

```
agent-quota-exporter --config FILE                         # serve /metrics and /healthz
agent-quota-exporter --config FILE --once                  # collect local sources once, print the metrics, exit
agent-quota-exporter --config FILE --once --once-with-quota  # also run the opt-in vendor sources if their gates allow
agent-quota-exporter --config FILE --check-config          # validate the config and exit 0 (valid) or 1; listens on nothing, writes nothing
agent-quota-exporter --version                             # version and metric contract version
```

- `--config` defaults to the `AGENT_QUOTA_EXPORTER_CONFIG` environment variable. With no config at all, the
  built-in defaults below apply (auto-discovery only).
- `--once` takes the same state-dir lock as the daemon, so it refuses to run next to a running exporter. Point it at a
  copy of the config with its own `state_dir` for ad-hoc checks. Vendor calls stay subject to their persisted gates.
- Endpoints: `GET /metrics` (Prometheus text format) and `GET /healthz`.

## Configuration reference

YAML, strict: an unknown key is an error. Durations are Go duration strings (`90s`, `15m`, `2h`). A value that looks
like an inline secret (a vendor key, JWT, `Bearer` header or PEM block) is rejected; the exporter never takes a key in
its config. The fully commented example is [examples/config.yaml](examples/config.yaml).

### Top level

| Key | Type | Default | Meaning |
|---|---|---|---|
| `version` | int | `1` | config format version; must be `1` |
| `listen` | string | `127.0.0.1:9877` | HTTP listen address. A port in use fails startup |
| `host` | string | OS hostname | value of the `host` label |
| `state_dir` | path | `/var/lib/agent-quota-exporter` | the exporter's own state: read offsets, vendor-call gates, cached quota. The only place it writes |
| `labels` | map | `{}` | constant labels added to every series (for example `{env: lab}`). Names must not reuse a contract label; at most 4 |
| `discovery.mode` | `off` \| `add` \| `only` | `add` | `off`: configured profiles only; `add`: configured plus discovered; `only`: discovered only |
| `discovery.roots` | list of globs | `["/root", "/home/*"]` | home dirs searched one level deep |
| `metrics.max_models_per_profile` | int | `20` | model label cap per profile; the rest fold into `model="other"` |
| `metrics.account_label` | `alias` \| `email` | `alias` | `alias`: configured `account`, else a short stable hash `acct-xxxxxxxx` of the account id; `email`: the login email (publishes email addresses to every metrics reader) |
| `metrics.per_session` | bool | `false` | one series group per live Claude Code session (state, waiting reason, since, info) |
| `metrics.max_sessions_per_profile` | int 1..500 | `50` | cap for per-session series |
| `defaults.usage_interval` | duration, >= 30s | `60s` | poll interval of local usage sources and of the status line cache |
| `defaults.stale_after` | duration | `45m` | minimum staleness bound, see [Freshness](#freshness) |
| `defaults.statusline_stale_after` | duration | unset | accepted for compatibility and ignored |
| `profiles` | list | `[]` | workspaces to read, see below |
| `sources.<name>` | map | | per-source overrides, see below |
| `jobs.lock_dir`, `jobs.call_log` | absolute paths | unset (off) | enable `job-locks`; see [specs/sources.md](specs/sources.md), "Jobs" |
| `strata` | list | `[]` (off) | model-server lanes, see below |
| `events` | map | disabled | optional local event history, see below |
| `otlp.endpoint` | string | unset | reserved; setting it fails validation (OTLP export is not implemented) |

### Profiles

| Key | Harness | Meaning |
|---|---|---|
| `harness` | all | `claude-code`, `codex`, `opencode`, `9router`, `agy` or `zai`. Another value is skipped with a warning |
| `alias` | all | the `profile` label; unique per harness |
| `dir` | all | the profile directory: `CLAUDE_CONFIG_DIR`, `CODEX_HOME`, the opencode data dir (holding `opencode.db`), the `.9router` dir, the agy `HOME`, or for `zai` the key holder's directory |
| `account` | all; required for `zai` and for `agy` with `agy_usage` | the `account` label. Default: a short stable hash of the account id where the harness has one |
| `disable` | all | `true` skips this profile and stops discovery from adding the same dir |
| `quota.claude_oauth_usage` | `claude-code` | `true` enables `claude-oauth-usage` |
| `quota.claude_statusline_cache` | `claude-code` | absolute path of a status line cache file; enables `claude-statusline-cache`. Unset by default (off) |
| `quota.codex_usage` | `codex` | `true` enables `codex-usage` |
| `quota.agy_usage` | `agy` | `true` enables `agy-usage` |
| `quota.zai_usage` | `zai` | must be `true` on a `zai` profile |
| `key_source` | `zai` | which local login holds the key: `openclaw`, `opencode` or `zai-helper` |
| `url` | `opencode` | address of a running opencode server; enables `opencode-http` |
| `allow_remote` | `opencode` | `true` allows a non-loopback `url` |

### Per-source overrides (`sources.<name>`)

| Key | Meaning |
|---|---|
| `min_interval` | poll interval. May not go below the source's floor |
| `backoff_429` | wait after a rate-limit answer (for `agy-usage`: after any failed run). May not go below the floor |
| `i_know_this_can_lock_my_account` | `true` lifts the floors for this source. Vendor endpoints lock out pollers; use only for testing |
| `byte_budget`, `deadline` | `claude-transcripts` only: bytes (0, or at least 64 MiB) and time per collection cycle, to bound catch-up after a long outage |

Defaults and floors of the vendor-calling sources:

| Source | Default interval | Floor | Default 429 backoff | Floor |
|---|---|---|---|---|
| `claude-oauth-usage` | 15m | 15m | 60m | 60m |
| `codex-usage` | 3m | 1m | 10m | 5m |
| `agy-usage` | 30m | 2m | 60m | 60m |
| `zai-usage` | 10m | 5m | 30m | 10m |

Local sources poll on `defaults.usage_interval` (transcripts, rollouts, opencode, router), every 60 s
(`claude-account`) or every 15 s (`claude-sessions`, `opencode-http`, `job-locks`); their floors are 30 s for file and
database readers and 5 s for session and job state.

### Claude status line cache

`claude-statusline-cache` lets a Claude Code status line script that already fetches the quota share it with the
exporter, so the exporter itself makes no vendor call. It is off unless the profile sets
`quota.claude_statusline_cache` to an absolute path. The file must be JSON of this shape:

```json
{"timestamp": 1767232800000, "status": "available", "data": { "...": "the body of Claude's usage endpoint response" }}
```

- `timestamp` is the observation time in Unix milliseconds; `status` must be `available`; `data` is parsed exactly
  like the `claude-oauth-usage` response.
- The file must be a regular file (no link), at most 1 MiB, owned by root, by the user the exporter runs as, or by the
  owner of the profile directory, and not group- or other-writable. Otherwise the source reports
  `reason="untrusted_owner"`.
- The profile's `.credentials.json` is only stat-ed, never read: a reading older than its modification time (a new
  login since) reports `auth_expired`.
- With `claude_oauth_usage: true` on the same profile, a valid cache reading at most 5 minutes old defers the vendor
  call; when the cache stops updating, the vendor source takes over within its own 15-minute floor. The newer of the
  two readings is exported.
- Under the shipped systemd unit (`PrivateTmp=true`) a file in the host's `/tmp` is not visible. Keep the file
  elsewhere, or bind its directory read-only into the service (a single-file bind can keep an old inode when the
  writer replaces the file atomically).

### zai profiles

The exporter never asks for a z.ai key. A `zai` profile names a login that is already on the machine and its
directory; the exporter reads the key from that file, read-only, sends it to `api.z.ai` only, and never logs or
stores it. Two profiles holding the same key are read once.

```yaml
- harness: zai
  alias: zai-openclaw
  account: zai-main               # the account label; required
  key_source: openclaw            # openclaw | opencode | zai-helper
  dir: /home/alice/.openclaw      # the holder's directory (opencode: its data dir; zai-helper: ~/.chelper)
  quota: { zai_usage: true }
```

### Strata lanes

```yaml
strata:
  - name: lane-a                       # the lane label; unique
    base_url: http://127.0.0.1:8080    # HTTP(S), no credentials, query or fragment
    interval: 30s                      # default 30s, minimum 5s
```

Read-only `GET /slots` and `GET /metrics` (JSON) on a model server that exposes them; no credentials, no inference
calls. See [specs/sources.md](specs/sources.md), "Strata".

### Events (optional local history)

Off by default. When enabled, the exporter appends structural facts (usage rows, identity, quota observations and
attempts) to a local append-only store, so history survives Prometheus retention. It never stores message content or
credentials.

| Key | Default | Meaning |
|---|---|---|
| `events.enabled` | `false` | turn the store on |
| `events.root_dir` | none | absolute directory for the store, separate from agent state; required when enabled |
| `events.byte_budget` | none | storage allocation in bytes; required and positive when enabled |
| `events.free_space_reserve` | `8388608` | bytes kept free on the filesystem (minimum 8 MiB) |
| `events.hot_age` | `2160h` | minimum 90 days; nothing is deleted automatically |
| `events.privacy_profile` | `community` | `community`: events are optional (`enabled: false` allowed). `parity`: requires `enabled: true` (fails validation when capture is disabled). Both capture the exact same fields and currently share identical runtime behaviour; retention and archiving are the operator's responsibility ([details](specs/event-contract.md#configuration-profiles)) |
| `events.identity_key_file` | unset | removed setting: accepted so older configs load, ignored with one startup warning |

The format is [specs/event-contract.md](specs/event-contract.md). Store capacity is exposed as
`agent_event_store_*` metrics.

### Freshness

A reading is stale when its age exceeds `max(defaults.stale_after, 2*P + D + 15s)`, where `P` is the source's
effective poll interval and `D` its collection timeout (10 s unless the source sets its own). Stale values are absent,
never zero; failed polls never renew a reading's observation time; a quota reset can make a value ineligible earlier.

## Metrics

All names, types, units and labels are defined in [specs/metric-contract.md](specs/metric-contract.md), the
authoritative contract. Base labels: `host harness profile account provider`.

| Family | Metrics |
|---|---|
| Quota (gauges) | `agent_quota_utilization_ratio`, `agent_quota_reset_timestamp_seconds`, `agent_quota_limit_info`, `agent_quota_observed_timestamp_seconds`, `agent_quota_credits_available`, `agent_quota_credits_balance`, `agent_quota_used_calls`, `agent_quota_limit_calls`, `agent_quota_reset_credits_available`, `agent_quota_reset_credits_next_expiry_timestamp_seconds` (labels add `window` and `scope`) |
| Usage (counters) | `agent_tokens_total` (`model`, `type`), `agent_requests_total` (`outcome`), `agent_request_errors_total` (`code`), `agent_cost_usd_total` (as the harness reports it; not a bill) |
| Account and state | `agent_account_logged_in`, `agent_account_info` (`plan`), `agent_credential_expiry_timestamp_seconds`, `agent_sessions` (`state`), `agent_router_connection_enabled`, `agent_router_connection_up`, `agent_router_rate_limited_until_timestamp_seconds` |
| Sessions and jobs (opt-in) | `agent_session_state`, `agent_session_waiting`, `agent_session_state_since_timestamp_seconds`, `agent_session_info`, `agent_job_running`, `agent_job_last_end_timestamp_seconds`, `agent_job_last_exit_code` |
| Lanes (opt-in) | `agent_lane_busy`, `agent_lane_queued`, `agent_lane_slots`, `agent_lane_requests_total`, `agent_lane_info` (labels `host lane`) |
| Source health | `agent_source_up`, `agent_source_last_success_timestamp_seconds`, `agent_source_cache_age_seconds`, `agent_source_backoff_until_timestamp_seconds`, `agent_source_read_seconds`, `agent_source_errors_total` (`reason`) |
| Collector and event store | `agent_collector_*` (backlog, files, bytes, rows, errors), `agent_event_store_*` (size, available bytes, pressure, write success; only with events enabled) |
| Exporter | `agent_quota_exporter_build_info`, `agent_quota_exporter_profiles`, `agent_quota_exporter_state_epoch`, `process_start_time_seconds` |

Sample output (from synthetic fixtures) is in [testdata/golden/metrics.txt](testdata/golden/metrics.txt).

## Security and privacy

What the exporter reads, and what it never does:

| It reads | It never |
|---|---|
| Agent state directories of configured and discovered profiles (transcripts, session files, rollouts, SQLite databases), read-only | writes to transcripts, credentials, or SQLite database/`-wal` files; opens SQLite `mode=ro` (`immutable=1` when an idle WAL database has no sidecars, though SQLite writes read-marks to a writable `-shm` and a narrow writer-close race during open can create `-wal`/`-shm` sidecars) |
| Credential files, only in the opt-in vendor sources that need a token, only for one request | refreshes, mints, writes, logs, prints or exports a token; tokens never appear in a label, an error or a log line |
| Account ids, plan names and token expiry times from credential and account files | exports an email address unless `metrics.account_label: email` |
| Structural fields of transcripts (timestamps, model, token counts) | reads or exports prompts, completions, tool output, screen text or paths |
| `/proc/<pid>/stat` and `/proc/locks` for session liveness and job locks | sends anything anywhere except the vendor endpoints of sources you opted in to |

- **Vendor endpoints.** `claude-oauth-usage`, `codex-usage` and `zai-usage` call private or undocumented vendor
  endpoints with a token the harness already holds. They are opt-in per profile, rate-gated per account, and back off
  after a rate-limit answer. Endpoints can change or rate-lock without notice.
- **agy.** `agy-usage` runs the agy client's own `/usage` command as the owner of the agy `HOME`. Like any agy run,
  it may refresh the client's login and rewrite its token file. This is the one opt-in source that can cause a
  credential write, by the vendor client, not by the exporter.
- **Privileges.** The exporter needs read access to the profiles it monitors and write access only to `state_dir`
  (and `events.root_dir` when enabled). Run it as a dedicated user with read-only group access to the profile dirs;
  run it as root only when the profiles are root's.
- **Network exposure.** `/metrics` listens on `127.0.0.1` by default. Labels carry account aliases (or emails if you
  choose); expose the port only to your Prometheus.
- **Symbolic links.** Discovered dirs, session and transcript files and SQLite sidecars that are links are refused,
  so a user cannot point the exporter at another user's files.

## Running under systemd

[examples/agent-quota-exporter.service](examples/agent-quota-exporter.service) is a hardened unit
(`ProtectSystem=strict`, `ProtectHome=read-only`, `PrivateTmp=true`, `NoNewPrivileges=true`). It runs as a dedicated
`agent-quota-exporter` user:

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin agent-quota-exporter
sudo install -D -m 0644 config.yaml /etc/agent-quota-exporter/config.yaml
sudo install -m 0644 agent-quota-exporter.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now agent-quota-exporter
```

The shipped [examples/config.yaml](examples/config.yaml) enables only profiles usable by the dedicated
`agent-quota-exporter` user once given read-only access to those profile dirs (for example via a read-only group).
Profiles under `/root` and other-user `agy_usage` profiles are included there as commented-out alternatives: using
them requires switching the unit to `User=root` and removing `Group=` (unless an `agy` `HOME` belongs to
`agent-quota-exporter`). The comments in the unit also cover the `PATH`, `ReadWritePaths=` and `MemoryMax=` changes
`agy-usage` needs.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). In short: `go vet ./... && go test -race ./...` must pass; fixtures are
synthetic; metric names come from [specs/metric-contract.md](specs/metric-contract.md).

| Path | What |
|---|---|
| `cmd/agent-quota-exporter` | the binary |
| `internal/` | `config` (load, validate, discover), `core` (scheduler, state, exposition), one package per harness (`claude`, `codex`, `opencode`, `router`, `agy`, `zai`, `strata`, `jobs`), `events`, `safefs` and `sqlro` (no-link file access, allow-listed read-only SQLite), `app` (wiring and end-to-end tests) |
| `specs/` | metric contract, event contract, per-source details, design notes and decisions |
| `fixtures/`, `testdata/` | synthetic sample inputs and golden output |
| `examples/` | example config and systemd unit |

Releases: pushing a tag `vX.Y.Z` (or `vX.Y.Z-rc.N`) on `main` builds the static tarballs and `SHA256SUMS`.

## Security reports

Please report vulnerabilities privately; see [SECURITY.md](SECURITY.md).

## License

[Apache-2.0](LICENSE), like node_exporter and the other Prometheus exporters.
