# Plan

Status: design phase, 2026-10-28. Nothing here is implemented. Companion documents: [metric-contract.md](metric-contract.md),
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
        | claude-code   |   | anthropic-oauth-   |
        | codex         |   | usage, codex-rate- |
        | opencode      |   | limits, billing    |
        | agy           |   | APIs (later)       |
        +---------------+   +--------------------+
```

**Core** (no vendor knowledge): config loader, profile discovery, source scheduler with per-source minimum interval and
cache, state store (offsets), the metric registry and label normaliser, cardinality caps, secret-redacting logger,
`/metrics` and `/healthz`, optional OTLP push.

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
  per_session: false              # cardinality hazard if true
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
    alias: lane-codex
    dir: /home/acct-c/.codex      # CODEX_HOME
    account: acct-c1
    quota: { codex_rate_limits: false }   # opt-in, risky until measured
  - harness: opencode
    alias: lane-1
    dir: /var/lib/opencode-lane-1/.local/share/opencode
    url: "http://127.0.0.1:4096"  # optional live source
    account: acct-d1
  - harness: agy
    alias: lane-agy
    dir: /home/acct-e
sources:                          # per-source overrides
  claude-oauth-usage: { min_interval: 15m, backoff_429: 60m }
  codex-rate-limits:  { min_interval: 15m }
otlp:
  endpoint: ""                    # empty = off
```

Config validation fails loudly at start (unknown keys, duplicate aliases, an inline secret-looking value, an
`min_interval` below the source's floor). Floors are compiled in and cannot be lowered below the vendor-safe value
without `i_know_this_can_lock_my_account: true`.

The parity design uses the same public account labels as metrics and requires an explicit local event-store
byte/reserve/age budget; their authoritative requirements are in
[event-contract.md](event-contract.md#persistence-privacy-and-freshness). These requirements are
not implemented config options yet. Active direct Claude freshness uses that contract's margin
policy rather than a threshold equal to its poll floor.
Compatibility: `events.identity_key_file` is accepted but ignored, with one log message
at startup when configured; no key is read or required.

### Scrape-time collection, intervals, cache

- `/metrics` **never blocks on a vendor call**. A scrape renders the current cache. Each source has a worker slot
  that refreshes its cache when `now - last_attempt >= min_interval` (jittered), triggered by a scrape or a timer,
  whichever is first. Prometheus can scrape every 15 s; the vendor is called at most once per `min_interval` per
  account.
- Floors: Claude quota 15 min per account (60 min after a 429); Codex rate limits 15 min per account; local
  files and SQLite 1 to 5 min incremental; opencode HTTP cached 30 s.
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

- One static binary (Go) or one zipapp/wheel (Python), plus a systemd unit, an example config, and a container image.
- `.deb`/`.rpm` or a tarball via release automation; a Helm chart is not planned.
- Reproducible builds, signed release artifacts, SBOM. Dependencies pinned and kept few.
- Ansible role / docs snippet for deploy are the consumer's job; the repo ships only the unit and config example.

## Go vs Python (decision #13 is OPEN; this is a recommendation, not a decision)

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
legitimate choice. The contract and fixtures are language-neutral, so the decision does not block phase 0 and 1.

## Phases and acceptance criteria

### Phase 0: contract and fixtures (this repo, now)
Deliverables: README, plan, metric contract, sources, sanitized fixtures, licence, ledger.
Acceptance: fixture secret-scan commands from `fixtures/README.md` return empty; the contract names every metric any
later phase emits; the maintainer confirms the open questions (licence, language) or explicitly defers them.

### Phase 1: core skeleton and the local-file plugins
Scope: config loader and validation, profile model and discovery, scheduler/cache, state store, `/metrics`, `/healthz`,
health metrics, Claude transcripts + sessions plugin, opencode DB plugin. No network calls.
Acceptance:
- Golden-file tests: feeding `fixtures/` produces the exact expected `/metrics` text for the contract metrics.
- Two Claude profiles and two opencode profiles on one machine produce disjoint, correctly labelled series.
- Counters are monotonic across restarts with state kept; reset cleanly (no replay) with state deleted.
- A fixture line with message content produces no content in any output, log or error (tested).
- Query allow-list test: the opencode plugin issues no statement touching `account`, `credential`, `part`.
- No write occurs under any profile directory (tested with a read-only mount).

### Phase 2: quota for Claude (opt-in, rate-gated)
Scope: `claude-oauth-usage` plugin with rate gate, backoff, 401 handling, per-account de-duplication; staleness drop.
Acceptance:
- Against a stub server: one request per account per 15 min regardless of scrape rate; 429 triggers 60 min backoff;
  two 401s stop attempts until the credential file changes.
- The access token never appears in argv, logs, errors, `/metrics`, or a heap/log dump in tests (canary-token test).
- The plugin never writes the credential file and never calls a refresh endpoint (network stub asserts only the usage
  host and path are contacted).
- Manual check on a real, logged-in profile (maintainer-run): ratios match the vendor's own display.

### Phase 3: Codex and agy discovery
Scope: Codex state/logs schema discovery, `codex-rate-limits` behind opt-in after measuring whether a single read can
trigger a token refresh, agy local-state discovery.
Acceptance: a written measurement (in `specs/sources.md`) of Codex refresh behaviour with evidence; if a read can
cause a refresh, the plugin ships off by default with that warning, or is dropped; agy gets either a real source or an
explicit "no surface" statement.

### Phase 4: provider billing plugins and OTLP
Scope: optional Anthropic/OpenAI admin-usage plugins and a balance plugin; OTLP push.
Acceptance: keys only via `env:`/`file:` references; OTLP output matches the contract names; plugin enable/disable by
config without a rebuild of the contract.

### Phase 5: packaging and first public release
Scope: release automation, systemd unit, container image, signed artifacts, example dashboards and alert rules,
final licence confirmation, public repository flip.
Acceptance: install on a clean VM from the release artifact in under 10 minutes following the README; security review
of the secret-handling paths; docs reviewed by someone who did not write them.

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
| A second refresher invalidates a grant | no refresh code exists in the codebase; network stub test; Codex plugin gated until measured |
| Local file formats change | per-plugin parser tests on fixtures, tolerant parsing, `parse` error counter |
| Cardinality blow-up | no ids as labels, model cap, per-session off by default |
| Licence ambiguity of prior art | no code borrowed |

## Open questions for the maintainer

1. ~~Licence~~: LOCKED Apache-2.0, 2026-10-28 (see `decisions.md`).
2. Language: Go or Python (recommendation above; decision #13).
3. Public repository timing (private until phase 5?).
4. Whether the Claude quota plugin should be on by default for discovered profiles, or always require explicit opt-in
   (this plan: always explicit).
5. Whether a per-session metric set is wanted at all.
