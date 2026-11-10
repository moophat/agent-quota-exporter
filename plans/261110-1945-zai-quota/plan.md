---
title: z.ai (Zhipu GLM Coding Plan) quota source
description: Opt-in `zai-usage` source that reads a z.ai key a local harness already holds and reports the Coding Plan quota windows. z.ai only.
status: draft
priority: P1
branch: feat/zai-quota
tags: [backend, go, prometheus, exporter, quota]
created: 2026-11-10
author: claude-sonnet-5-5 (planner), at the orchestrator's request
---

# Plan: z.ai quota

Owner (Tu, 2026-11-10, via `/root/orchestrator/plans/261110-1924-zai-quota/brief.md`): the exporter "should not ask user for a
key. It ask for 'what harness and login do you have on this box, can i use those file' and work from there"; "someone do
ak:plan, then someone do ak:cook properly. That's for z.ai alone". Scope is z.ai only; the vendor-endpoints file is out.

## Outcome and acceptance criteria

A user who logged in to z.ai through OpenClaw, opencode or the z.ai coding helper can opt a profile in and see the Coding Plan
quota on `/metrics`, without ever giving the exporter a key and without OAuth.

1. `go test ./...` and `go vet ./...` pass with `PATH=/usr/local/go/bin:$PATH`; no test makes a live call.
2. A config with one `harness: zai` profile per supported key source loads; every invalid shape in phase 1 fails loudly.
3. With a fixture key file and a fake HTTPS transport, `/metrics` shows the series of "Metrics" below, with the labels below.
4. Same key held by two harnesses: one request per poll, one set of series, the other profile silent (phase 5 test).
5. The key never appears in any log line, error, metric, label, state file, event, argv or test output (canary test, phase 5).
6. Failure table of "Failure reasons" is covered by a test per row.
7. `specs/sources.md`, `specs/metric-contract.md`, `README.md`, `CHANGELOG.md`, `fixtures/README.md` updated (phase 6).
8. Live check (maintainer, not CI): on U02 with the OpenClaw profile the series match what the key's own
   `GET /api/monitor/usage/quota/limit` returns (verified 200 on 2026-11-10: level `lite`, three limits).

## Non-goals

- Asking for, storing, inlining or resolving a key from the exporter config (`env:`/`file:` stay unresolved; the scalar secret
  scanner keeps refusing inline keys).
- OAuth, token refresh, logging in, or calling any z.ai endpoint other than `GET /api/monitor/usage/quota/limit`.
- `model-usage` (returns no data), per-tool `usageDetails` series (cardinality), credits/money (D9), events for this source
  (event-contract unchanged in this plan; open question 5), zcode (below), arbitrary `base_url` (keeps the key from being sent
  to a host the operator typo'd), discovery of z.ai profiles, the vendor-endpoints file.

## Design

### Profile model: a new harness `zai`

Audit (`/root/orchestrator/plans/reports/exporter-config-knobs-audit-261110.md` section 4) found `harness` names a client and
`opencode` is taken by the opencode DB source, so "opencode holds the z.ai key" cannot be a second source on that profile.
z.ai is a vendor reached through several clients, so it gets its own harness whose profile says where the login lives:

```yaml
profiles:
  - harness: zai
    alias: zai-openclaw          # unique per harness, becomes the `profile` label
    account: zai-main            # REQUIRED; the `account` label. Never derived from the key.
    key_source: openclaw         # REQUIRED: openclaw | opencode | zai-helper
    dir: /home/juniper/.openclaw # REQUIRED: the holder's directory (meaning per key_source, below)
    region: global               # optional: global | cn. Default: from the key source, else global.
    quota: {zai_usage: true}     # REQUIRED true; the opt-in. A zai profile without it is a config error.
```

- `Profile` gains `KeySource string yaml:"key_source"` and `Region string yaml:"region"`; `Resolved` carries both. Unknown YAML keys
  still fail (`KnownFields`). `key_source`/`region` are rejected on any harness other than `zai`.
- Never discovered (like agy and 9router). `disable: true` works as for others.
- `quota.zai_usage` is the only quota key; `quotaKeys["zai_usage"] = zai`.
- `Floors["zai-usage"] = {MinInterval: 5m, Backoff429: 10m}`; defaults 10 min interval, 30 min 429 backoff (judgment, the
  endpoint is undocumented and community-used; operator may raise via `sources.zai-usage.*`, lowering below the floor needs
  `i_know_this_can_lock_my_account` as for other sources).
- Network: `NetGroup() == "zai"`, `NetSpacing() == 5s`, so a z.ai 429 never pauses Claude/Codex/agy.
- Region to host (constants, no knob): `global` -> `https://api.z.ai`, `cn` -> `https://open.bigmodel.cn`. Path
  `/api/monitor/usage/quota/limit`. The cn host is **unverified** for this path (brief: same API family).

### Key sources (read-only, one file, one request, never logged)

`dir` is the holder directory; the filename and field are fixed per source, reviewed code (same posture as `.credentials.json`
today). All opens use `safefs.RealDir` / `safefs.OpenRegular` (no links, regular files, size cap 1 MiB); buffers are zeroed
after decode. The key must be printable ASCII without whitespace, length 16..512, else `parse`.

| `key_source` | `dir` is | File and field | Region hint | Evidence |
|---|---|---|---|---|
| `openclaw` | `$OPENCLAW_STATE_DIR` (default `~/.openclaw`) | `<dir>/openclaw.json` (**JSON5**: the real file on U02 has `//` comment lines, so a plain JSON decoder fails). First present, in order, of `models.providers.zai.apiKey`, `models.providers.zai-anthropic.apiKey` (a string), then `env.vars.ZAI_API_KEY` | host of `models.providers.zai.baseUrl`: `api.z.ai` -> global, `open.bigmodel.cn` -> cn | **verified**: structure of `/home/juniper/.openclaw/openclaw.json` read by key names on 2026-11-10; key works on the endpoint (brief). The brief wrote `providers.zai.apiKey`; the real path has the leading `models.`. `env.vars` fallback is **documented** (OpenClaw `docs/providers/zai.md`), not seen live |
| `opencode` | the opencode **data** dir (`~/.local/share/opencode`, same value as the `opencode` harness profile) | (a) legacy `<dir>/auth.json`: `{"<integrationID>": {"type":"api","key":"..."}}`; (b) current versions: `<dir>/opencode.db` table `credential` (`integration_id`, `value` JSON text, `active`, `time_updated`), key at `$.key` where `$.type == "key"`. Integration ids in order: `zai-coding-plan`, `zhipuai-coding-plan`, `zai`, `zhipuai`. File (a) wins when it has a match; else (b) | `zai-coding-plan`/`zai` -> global; `zhipuai-*` -> cn | **verified** from opencode source (`packages/core/src/database/migration/20260805200742_import_legacy_credentials.ts`, `packages/core/src/credential/sql.ts`, read 2026-11-10) and the local `opencode.db` schema (opencode 1.18.33 has the `credential` table, no rows on this node). Provider ids `zai-coding-plan`/`zhipuai-coding-plan` are what the z.ai helper writes. Legacy `auth.json` path is from the migration code. **Not verified**: a real z.ai row (none on this node) |
| `zai-helper` | `~/.chelper` | `<dir>/config.yaml`: `api_key` (string), `plan` (`glm_coding_plan_global` or `glm_coding_plan_china`). Parsed with the existing `gopkg.in/yaml.v3` | `plan` | **verified** from the package source (`@z_ai/coding-helper` 0.1.1 `dist/lib/config.js`, `commands/auth.js`, read 2026-11-10). **Correction to the brief**: the helper keeps its key in `~/.chelper/config.yaml`, not `~/.zai`. The helper also writes the key into `~/.config/opencode/opencode.json` `provider.<id>.options.apiKey` and `~/.codex/config.toml`; the exporter reads only `~/.chelper` (open question 3) |
| `zcode` | n/a | **unsupported, rejected at config load.** zcode's docs (`zcode-app-cli` 3.17.2-45, `docs/CONFIGURATION.md`) say `~/.zcode/v2/credentials.json` is written by the runtime, the OAuth login is a Desktop "business token", and "credential validation and decryption belong to the runtime". It is not a plain key, and nothing shows it is accepted by `/api/monitor`. Revisit only with proof | n/a | **documented**; unproven |

Region precedence: config `region` > key-source hint > `global`. A key whose hint and host disagree is the operator's problem, not
guessed.

### Duplicate keys: one account is read once

Profiles are compared by what they hold, not by name. Every zai source gets a shared `peers` set (built in `app.Build`, like
`claude.QuotaCache`). Before any request a source calls `peers.owner(self)`: it reads each peer's key (local, cheap), hashes in
memory (`sha256`, never stored or logged), and among the peers with an equal hash the owner is the one with the
lexicographically smallest `alias`. A non-owner makes no request, emits no quota series, reports `agent_source_up 1` with no
error, and logs one `slog` warning per process naming only the two aliases. This is deterministic across restarts and needs no
persisted state. A peer whose key cannot be read is ignored in the comparison. Two different keys on one account is not detectable
and is out of scope. Within one holder, two entries with the same value (`zai` and `zai-anthropic` in OpenClaw) are the same key
by construction and are not an error.

### Metrics emitted

Labels: `host harness="zai" profile account provider="zai" window scope`. All series follow the shared freshness rule
(`core.FreshnessLimit`) and are served from the persisted last good reading after a failure or restart until stale.

| Metric | When |
|---|---|
| `agent_quota_utilization_ratio` | per mapped limit: `percentage / 100`; for `TIME_LIMIT` with no `percentage`, `currentValue / usage` (guard `usage > 0`); absent otherwise, never 0 |
| `agent_quota_reset_timestamp_seconds` | `nextResetTime` (ms) / 1000, only when present and plausible; a window not started (the 5h window in the fixture) has none |
| `agent_quota_observed_timestamp_seconds` | `harness profile account provider`: time of the successful read |
| `agent_quota_used_calls` **new** | `TIME_LIMIT` only: `currentValue` |
| `agent_quota_limit_calls` **new** | `TIME_LIMIT` only: `usage` (the cap; z.ai names the cap `usage`, the used count `currentValue`) |
| `agent_account_info` | `plan` = `data.level` lowercased, one of `lite`, `pro`, `max`, `team`, else `unknown` (contract adds `lite`) |
| standard `agent_source_*` | `source="zai-usage"`, `harness="zai"` |

Window mapping (`type`, `unit`, `number`), deliberately closed:

| z.ai limit | `window` | `scope` | Basis |
|---|---|---|---|
| `TOKENS_LIMIT`, unit 3 (hour), number n | `<n>h` (fixture: `5h`) | `all` | unit 3 = hour is **inferred** from the live response (unit 3 number 5) |
| `TOKENS_LIMIT`, unit 6 (week), number n | `<7n>d` (fixture: `7d`) | `all` | unit 6 = week **inferred** (unit 6 number 1 with a reset one week out) |
| `TIME_LIMIT`, unit 5 (month), number 1 | `monthly` | `tool_calls` | unit 5 = month **inferred**; only number 1 is mapped |
| anything else (new type, new unit, `number` not a positive integer, label over 16 chars) | not exported | | counted once per read in `agent_source_errors_total{reason="parse"}`; the read still succeeds (same convention as agy unknown buckets) |

Two limits mapping to the same label set merge (highest ratio, earliest reset). The cockpit shows `5h`/`7d` like other vendors.

### Failure reasons (contract vocabulary only, no free text)

| Condition | Reason | Behaviour |
|---|---|---|
| `dir` missing / key file missing / no matching entry in file or DB | `not_found` | previous reading served until stale |
| link at any path component, unreadable, non-regular file | `permission` | same |
| file over 1 MiB, malformed JSON5/YAML/JSON, key not a string (SecretRef object, env reference), key fails the shape check, response body not the expected envelope | `parse` | same |
| HTTP 401 / 403, or HTTP 200 with `success:false` and an auth-looking `code` (1000-1004, **unverified**: z.ai API code table not checked) | `auth_expired` | two in a row with the same key-file fingerprint stop calls until the file changes (no refresh exists); `status 401` never retried inline |
| HTTP 429, or 200 with `success:false` and code `1302`/`1305` (from OpenClaw's z.ai doc: rate limit / overload) | `rate_limited` | `SourceError.Backoff` = `sources.zai-usage.backoff_429` (default 30 min, floor 10 min), in group `zai` |
| other 4xx/5xx, TLS/DNS failure, body over 1 MiB, redirect | `upstream` | next normal interval |
| deadline | `timeout` | next normal interval |
| 200 with `success:true` but no `data.limits` | `parse` | no series, never a fake 0 |
| non-owner of a duplicate key | none | silent, see above |

### Red-team notes (inline; the four lenses of ak:plan)

- Assumptions: the endpoint is undocumented and may change; mitigated by the closed mapping, `parse` accounting, and `unit`
  inference marked in the spec as inferred. `Authorization: <key>` without `Bearer` is verified live; do not add `Bearer`.
- Failure: a key rotated in the holder is picked up on the next poll (re-read per call). A revoked key stops after two auth
  failures, restarts when the file fingerprint changes. A holder file being rewritten mid-read yields `parse`, retried.
- Scope: every extra the brief did not ask for (events, usageDetails, model-usage, base_url, discovery, zcode) is a non-goal.
- Security: the key can only go to two constant hosts over HTTPS with the system trust store, no proxy (as `codex-usage`),
  redirects refused, header set from a local variable and deleted after `Do`; config scanner unchanged; `credential` DB table
  read is a deliberate, narrow exception to the "never read" list (one table, three expressions, `ByKeys` on four ids).

## Phases

| # | File | Deliverable | Depends |
|---|---|---|---|
| 1 | [phase-01-config.md](phase-01-config.md) | `zai` harness, `key_source`/`region`/`zai_usage`, validation, floors, Resolve | none |
| 2 | [phase-02-key-readers-files.md](phase-02-key-readers-files.md) | key reader seam + `openclaw` (JSON5-lite) + `zai-helper` readers | 1 |
| 3 | [phase-03-key-reader-opencode.md](phase-03-key-reader-opencode.md) | `opencode` reader: `auth.json` then `credential` table | 2 |
| 4 | [phase-04-parse-map.md](phase-04-parse-map.md) | response parser, window mapping, samples, new families | none (parallel with 2-3) |
| 5 | [phase-05-source-wiring.md](phase-05-source-wiring.md) | `zai-usage` source (HTTP, state, breaker, backoff, peers dedupe), `app.Build`, e2e | 1-4 |
| 6 | [phase-06-docs-acceptance.md](phase-06-docs-acceptance.md) | specs, README, CHANGELOG, fixtures README, final sweep | 5 |

Package: `internal/zai/` (new). Files owned per phase are listed in each phase. Phases 2/3 and 4 touch disjoint files.
Builder conventions: `AGENTS.md` rules 0-12; tests first; signed conventional commits without `Co-Authored-By` and without plan
or phase labels in code, test names or messages.

## Open questions

1. Is HTTP 200 + `success:false` the shape of an invalid key on this endpoint? (Not seen; phase 5 tests treat both forms.)
2. cn host `open.bigmodel.cn` for `/api/monitor/usage/quota/limit`: unverified. Ship `cn` anyway, flagged in the spec?
3. Also read `~/.config/opencode/opencode.json` `provider.zai-coding-plan.options.apiKey` (where the helper writes it) as a fourth
   source? Adds a second dir concept; planned out.
4. Default interval 10 min / floor 5 min: confirm, or take z.ai's own tolerance if the owner knows it.
5. Events (`event-contract.md`) for this source: left out; add later if the cockpit wants quota history.
6. OpenClaw keys held as a SecretRef or in its SQLite auth store (newer installs, `state/openclaw.sqlite`) are unsupported and
   report `parse`; worth a source later if any node uses them (U02 uses the inline key).

## Decisions on open questions (orchestrator FM02, 2026-11-10 19:3x +07)
1. Bad key: verified live with a fake key: HTTP 200 {"code":1000,"msg":"身份验证失败。","success":false}; no Authorization header: HTTP 200 code 1001 "Authentication parameter not received in Header". Treat success:false with code 1000-1004 as auth_expired (two-strike breaker as planned), any other success:false as upstream. HTTP 200 alone never means data.
2. cn region: not shipped (host unverified). Leave `region` out of config for now; global api.z.ai only.
3. ~/.config/opencode/opencode.json apiKey as a fourth source: no.
4. Intervals as planned: 10 min default, 5 min floor; 429 backoff 30 min, 10 min floor.
5. Events from this source: no.
6. OpenClaw SecretRef / SQLite key forms: not now (report parse as planned).
