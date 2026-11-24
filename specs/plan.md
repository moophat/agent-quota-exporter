# Plan

Status: the original design document (2026-10-28), kept for its rationale. Most of it is implemented; where it
disagrees with the code, [../README.md](../README.md), the contracts and [decisions.md](decisions.md) describe current
behaviour. Companion documents: [metric-contract.md](metric-contract.md),
[sources.md](sources.md), [../fixtures/README.md](../fixtures/README.md).

## Goal

One small, read-only process per machine that exposes, as Prometheus metrics, the subscription quota windows and the
per-agent usage of every AI coding-agent account on that machine, across vendors, with several accounts in several
workspaces as the normal case. It never refreshes or writes a token.

Non-goals: host metrics, billing reconciliation, inference health checks, a UI, a store, alert rules (examples may
ship later), knowledge of any particular fleet.

## Prior art

None of these has a detected licence, so ideas only, no code, and each is cited by idea:

- `jimyag/ai-token-exporter`: local log parsers for several agents, `/metrics`; no quota windows, no multi-account.
- `georgewhewell/quota-exporter`: quota windows as `llm_quota_utilization_ratio{provider,window,scope}`; calls private
  endpoints with CLI OAuth tokens and warns that refreshing can revoke live CLI sessions.
- `danielrigobertojs/ai-usage-exporter`: Claude Code, Codex, opencode usage, Grafana dashboards, pre-release.
- API-billing exporters (OpenAI, Anthropic admin APIs, balance exporters): a different surface (API keys), kept as
  optional provider plugins.

Borrowed: the ratio/window/scope idea; per-agent log locations as a starting point to re-verify. Not borrowed: token
refreshing, any code.

## Architecture

```
          Prometheus scrape (any interval)
                    |
              +-----v------+       +-----------------+
              |  HTTP      |       |  config + auto- |
              |  /metrics  |       |  discovery      |
              +-----+------+       +--------+--------+
                    |                       | profiles[]
              +-----v-----------------------v--------+
              |                CORE                  |
              |  registry | scheduler/cache | state  |
              |  label normaliser | cardinality caps |
              +--+-----------------+-----------------+
                 | Source API      | Source API
        +--------v------+   +------v-------------+
        | harness plugin|   | provider plugin    |
        | claude-code   |   | claude-oauth-usage |
        | codex         |   | codex-usage        |
        | opencode      |   | agy-usage          |
        | agy           |   | zai-usage          |
        +---------------+   +--------------------+
```

**Core** (no vendor knowledge): config loader, profile discovery, source scheduler with per-source minimum interval and
cache, state store (offsets), the metric registry and label normaliser, cardinality caps, secret-redacting logger,
`/metrics` and `/healthz`. (OTLP push was considered and is not built.)

**Plugins** are in-tree modules behind one narrow interface (no dynamic loading in v1):

- A **harness plugin** knows one agent's local state: how to find its profiles, read its sessions and usage, check
  login presence. It produces usage counters, session gauges and account state.
- A **provider plugin** knows one vendor's quota or billing surface: it takes a credential reference supplied by
  the harness plugin or config and produces quota gauges. It is the only code that ever holds a token.

Source interface (language-neutral): `Discover() -> []Profile`, `Collect(profile, now, budget) -> Samples | Error`,
each source declaring `name`, `min_interval`, `needs_credential: bool`, `opt_in: bool`. The core owns timing,
caching, backoff and metric emission; a plugin only reads and returns facts. Plugins cannot register metrics outside
the contract.

### Profiles

A profile = `{harness, alias, dir/home/data-dir, [instance url], [account alias]}`. Auto-discovery proposes profiles
from standard locations; the config file adds, renames, disables or overrides them.

### Discovery rules

- Per harness, check the standard locations: `~/.claude`, `$CLAUDE_CONFIG_DIR`; `~/.codex`, `$CODEX_HOME`;
  `$XDG_DATA_HOME/opencode` (default `~/.local/share/opencode`); the agy home.
- Scan a configurable list of parent roots (`discovery.roots`, default `/home/*`, `/root`) for the known directory
  names, one level deep, so per-account homes (`/home/acct-a/.claude`) are found. Never recurse into project trees.
- Read process environments? **No.** Reading `/proc/<pid>/environ` of other processes to find `CLAUDE_CONFIG_DIR` is
  invasive; non-standard locations must be listed in config. (Revisit only if operators ask.)
- A discovered profile gets `origin: discovered`, alias = slug of its directory name, and a one-time log line. Config
  can pin `discovery.mode: off | add | only` (`add` default).
- Two profiles resolving to the same credential (same account id) are flagged once and queried once.

### Config format

YAML, path from `--config` or `AGENT_QUOTA_EXPORTER_CONFIG`. Secrets are never inline; keys are referenced
(`env:NAME`, `file:/path`).

```yaml
version: 1
listen: "127.0.0.1:9877"
host: node-a                      # label value; default = OS hostname
state_dir: /var/lib/agent-quota-exporter
labels:                           # constant labels on every series (optional)
  env: lab
discovery:
  mode: add                       # off | add | only
  roots: ["/root", "/home/*"]
metrics:
  per_session: false              # opt-in: one series group per live session (specs/metric-contract.md)
  max_sessions_per_profile: 50    # cap when per_session is on
  max_models_per_profile: 20
  account_label: alias            # default; email publishes addresses in metrics and events
defaults:
  usage_interval: 60s
  stale_after: 45m
profiles:
  - harness: claude-code
    alias: work
    dir: /home/acct-a/.claude     # CLAUDE_CONFIG_DIR
    account: acct-a1
    quota: { claude_oauth_usage: true }   # opt-in, undocumented endpoint
  - harness: claude-code
    alias: personal
    dir: /home/acct-b/.claude
    account: acct-b1
  - harness: codex
    alias: codex-work
    dir: /home/acct-c/.codex      # CODEX_HOME
    account: acct-c1
    quota: { codex_usage: true }  # opt-in vendor call
  - harness: opencode
    alias: oc-a
    dir: /home/acct-d/.local/share/opencode
    url: "http://127.0.0.1:4096"  # optional live source
    account: acct-d1
  - harness: agy
    alias: agy-a
    dir: /home/acct-e
    account: agy-e1
    quota: { agy_usage: true }
sources:                          # per-source overrides
  claude-oauth-usage: { min_interval: 15m, backoff_429: 60m }
  codex-usage:        { min_interval: 3m, backoff_429: 10m }
```

The complete, current key list is the README's "Configuration reference".

Config validation fails loudly at start (unknown keys, duplicate aliases, an inline secret-looking value, an
`min_interval` below the source's floor). Floors are compiled in and cannot be lowered below the vendor-safe value
without `i_know_this_can_lock_my_account: true`.

The optional event store (`events:`) uses the same public account labels as metrics and requires an explicit
byte, reserve and age budget; its requirements are in [event-contract.md](event-contract.md#configuration-profiles).
Claude quota freshness uses that contract's margin policy rather than a threshold equal to its poll floor.

### Scrape-time collection, intervals, cache

- `/metrics` **never blocks on a vendor call**. A scrape renders the current cache. Each source has a worker slot
  that refreshes its cache when `now - last_attempt >= min_interval` (jittered), triggered by a scrape or a timer,
  whichever is first. Prometheus can scrape every 15 s; the vendor is called at most once per `min_interval` per
  account.
- Defaults and floors: Claude quota 15 min per account, floor 15 min (60 min after a 429); Codex live quota
  (`codex-usage`) every 3 min, floor 1 min (10 min after a 429, floor 5 min); agy every 30 min, floor 2 min; z.ai
  every 10 min, floor 5 min; local files and SQLite on `defaults.usage_interval` (60 s, floor 30 s), incremental;
  session state and opencode HTTP every 15 s, floor 5 s.
- Cache entries carry `observed_at`. A value with age exceeding `max(stale_after, 2*P + D + 15s)` is dropped
  (absent, not zero). P is effective poll interval; D is collection timeout (`ReadTimeout()`, default 10s).
  Exactly at the bound is fresh; `stale_after` accepts any positive duration. The legacy statusline
  setting is ignored. This applies to passive readings at their original event time as well.
- Concurrency: one in-flight read per source, a global cap on concurrent reads, a per-read timeout (default 10 s), and
  a file lock on the state dir so two exporter processes cannot run on the same profiles.
- Rate gates persist in `state_dir` (last attempt, backoff-until, consecutive 401 count), so a restart loop cannot
  hammer a vendor.

### Error handling

- Per source and profile: errors are classified into the bounded `reason` set (see contract), counted in
  `agent_source_errors_total`, shown by `agent_source_up == 0`, and logged once per state change (not per attempt).
- A failing source never fails the scrape or other sources. Partial output is normal.
- `401` on a quota source: mark `auth_expired`, keep serving nothing for that quota after the shared freshness bound, do **not**
  retry sooner than the floor, and stop after two consecutive 401s until the credential file's mtime or expiry changes.
- `429`: backoff (default 60 min), exposed in `agent_source_backoff_until_timestamp_seconds`.
- Parse errors: skip the record, count it, never print the offending line (it may hold content).
- Permission errors: reported with the path's directory only, never file contents.
- SQLite busy: retry within the read budget, then skip this cycle; never wait on a lock held by a live agent.
- Logging: structured, never includes request headers, tokens, response bodies or message text. A redaction filter is
  a defence in depth, not the primary control (the primary control is that those values never reach the logger).

### Security posture

- Runs as a dedicated user with read-only group access; systemd unit hardening in the package
  (`ProtectSystem=strict`, `ReadOnlyPaths`, `NoNewPrivileges`, no network except the vendor hosts used).
- `/metrics` binds to localhost by default; exposing it is an explicit config choice. No auth in v1 (put it behind the
  scraper's network or a reverse proxy).
- Tokens exist only in plugin-local variables for the duration of one request.

## Packaging

- Built: static Linux binaries (amd64, arm64) in release tarballs with `SHA256SUMS`, a systemd unit and an example
  config. Dependencies pinned and kept few.
- Not built yet: a container image, `.deb`/`.rpm` packages, signed artifacts and an SBOM. A Helm chart is not planned.
- Deployment tooling is the consumer's job; the repo ships only the unit and config example.

## Go vs Python (settled: Go, `decisions.md` D2)

| Concern | Go | Python |
|---|---|---|
| Deploy to many nodes | single static binary, no runtime | needs an interpreter and, unless stdlib-only, a venv per node |
| SQLite read-only access | pure-Go driver (`modernc.org/sqlite`) works without cgo; `mode=ro` supported | stdlib `sqlite3`, zero deps |
| Prometheus exposition | `client_golang`, standard, mature | `prometheus_client` (one dep) or hand-rolled text format (small, stdlib) |
| Long-running scheduler/cache/concurrency | goroutines, strong fit | asyncio/threads, fine at this scale |
| Subprocess/JSON-RPC (codex app-server) | fine | fine |
| Community contribution barrier | moderate, common in the exporter ecosystem | lower for casual contributors |
| Prototype speed | slower | faster |
| Supply-chain surface | small with few deps | small if stdlib-only, larger otherwise |
| Memory/CPU on a busy dev node | small, predictable | acceptable, but a persistent process holds more memory |

Recommendation: **Go**, because the exporter is a long-running, read-only daemon deployed to many heterogeneous nodes,
where a single static binary and predictable footprint outweigh Python's faster prototyping, and because most
Prometheus exporters are Go (contributors and operators expect it). A Python reference implementation of one
plugin could be written first to settle the parsers against the fixtures if prototyping speed matters, but it should not
become the product by drift. If the operator values zero build tooling and edits on the node, stdlib-only Python is a
legitimate choice. The contract and fixtures are language-neutral.

## Test strategy

- Fixture-driven unit tests per plugin (the committed fixtures are the oracle).
- Golden `/metrics` output tests per contract version.
- Property tests: no output string contains any fixture "canary" content or token.
- Stub servers for vendor endpoints (status codes, delays, 429/401 sequences).
- Cardinality test: N profiles x M models produce at most the documented number of series.

## Risks

| Risk | Mitigation |
|---|---|
| Vendor private endpoint changes or rate-locks | opt-in, floors, backoff, per-source health, graceful absence |
| Token accidentally logged | tokens never reach logger by construction, canary tests, no body logging |
| A second refresher invalidates a grant | no refresh code exists in the codebase; network stub test; Codex and agy vendor sources are opt-in |
| Local file formats change | per-plugin parser tests on fixtures, tolerant parsing, `parse` error counter |
| Cardinality blow-up | no ids as labels, model cap, per-session off by default |
| Licence ambiguity of prior art | no code borrowed |

## Questions raised by this plan, and their answers

1. Licence: Apache-2.0 (`decisions.md` D1).
2. Language: Go (`decisions.md` D2).
3. Public repository: yes (`decisions.md` D3).
4. Claude quota plugin on by default for discovered profiles: no, always an explicit opt-in per profile.
5. Per-session metrics: opt-in only (`decisions.md` D4).
