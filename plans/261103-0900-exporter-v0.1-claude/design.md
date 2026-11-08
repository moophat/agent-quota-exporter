# agent-quota-exporter — build plan v0.1 (Claude only)

Status: DRAFT for red team and Tu's go. Branch `feat/phase1-core`. Author: claude-opus-5-5 (cloud lane).
Authority: `specs/metric-contract.md` (contract 0), `specs/plan.md`, `specs/sources.md`, `specs/decisions.md`.
Tu's rulings 2026-11-02: no opencode in this lane; label map (agent-fleet-kit `mesh/`) after the exporter is up;
model id in every commit/PR; v0.1.0 = plain binaries + checksums; code-only here, on-prem agent integration-tests the PR.

## 1. Scope

In: spec phase 1 minus opencode, plus spec phase 2 (Claude quota). Everything else out.

| Piece | Spec phase | In v0.1 |
|---|---|---|
| config loader + validation, discovery, scheduler/cache, state store, `/metrics`, `/healthz`, health metrics | 1 | yes |
| `claude-transcripts`, `claude-sessions`, `claude-account` | 1 | yes |
| `claude-oauth-usage` (opt-in, rate-gated) | 2 | yes |
| opencode, codex, agy, billing, OTLP, per-session metrics | 1/3/4 | no (config keys for them: see 3.4) |
| systemd unit + example config | 5 | example files only, no packages/container/signing |
| CI (vet, test, fixture checks) + tag-triggered release (linux amd64/arm64 static tarballs + SHA256SUMS) | 1/5 | yes |

Why phase 2 now: the fleet retires `usage.json` (OD #28/#29) only once quota windows come from the exporter.

## 2. Shape

```
cmd/agent-quota-exporter/main.go   flags: --config, --once (collect all once, print /metrics, exit), --version
internal/config     YAML (gopkg.in/yaml.v3, KnownFields), validation, floors, discovery
internal/core       scheduler + cache + gates, state store (JSON, atomic write, flock), renderer, HTTP
internal/claude     transcripts, sessions, account, oauthusage
```
One third-party dependency: `gopkg.in/yaml.v3`. Exposition text is hand-written (no client_golang): small, exact
golden output, fewer deps (plan: "dependencies pinned and kept few").

Source interface (plan §Architecture): each source instance is bound to one profile;
`Name()`, `MinInterval()`, `Collect(ctx, now) ([]Sample, error)`. Errors carry a bounded `reason`
(contract list) and an optional backoff. The core owns timing, cache, staleness, health metrics, persistence.
Plugins return the full current value set (counters as cumulative totals they keep in their own state slot).

## 3. Core behaviour

3.1 Scheduler: one goroutine ticks every 5 s; an instance runs when `now - last_attempt >= min_interval` (+ up to
10 % jitter) and it is not in backoff. One in-flight read per instance, global cap 4 concurrent reads, per-read
timeout 10 s. `/metrics` renders the cache only, never blocks on a read.

3.2 Staleness: every cached plugin sample with age exceeding `max(stale_after, 2*P + D + 15s)` is dropped from
output: absent, never zero. Health metrics are always emitted.

3.3 State: `state_dir/state.json` = per instance {last_attempt, last_success, backoff_until, plugin blob} plus
`epoch` (creation time → `agent_quota_exporter_state_epoch`). Written atomically (tmp + rename) after each collect
that changed it. `state_dir/lock` held with flock for the process lifetime; a second process exits non-zero.
Gates persist, so a restart loop cannot hammer the vendor.

3.4 Config: shape as `specs/plan.md` §Config format. Fails loudly on: unknown keys, duplicate alias, inline
secret-looking value (`sk-`, `eyJ`, `Bearer `), `min_interval` below floor without
`i_know_this_can_lock_my_account: true`, static label colliding with a contract label or invalid name, > 12 labels.
Profiles for harnesses without a plugin (codex, opencode, agy): accepted, warned once, skipped (so one fleet config
works across versions). `otlp.endpoint` non-empty or `metrics.per_session: true`: startup error "not in this version".

3.5 Discovery (`discovery.mode` off|add|only, roots default `/root`, `/home/*`): one level under each root, dirs
named `.claude` or `.claude-*` that contain `projects/`, `sessions/` or `.credentials.json`; plus
`$CLAUDE_CONFIG_DIR` of the exporter's own env. Alias: for `.claude` the home's name (`root`, `acct-a`); for
`.claude-X` → `claude-x`; collisions get `-2`. A dir already in config is not re-added. Never recurse.
Deviation flagged: spec says "slug of the directory name", which makes every `~/.claude` collide as `claude`.

3.6 Renderer: fixed family order, series sorted by label string, HELP/TYPE per family, label escaping per the text
format, integral values printed as integers. Static labels appended to every series.

## 4. Claude plugins

4.1 `claude-account` (no own source; feeds labels and state metrics, re-read every 60 s):
- account id: `.claude.json` → `oauthAccount.accountUuid` only (email only if `account_label: email`). File is
  `<dir>/.claude.json`; for a default `~/.claude` dir it is the sibling `~/.claude.json`.
- `account` label: config alias, else `acct-` + first 8 hex of sha256(uuid), else `unknown`.
- `.credentials.json`: decode only `claudeAiOauth.expiresAt` and presence of `accessToken`; buffer zeroed after.
  → `agent_credential_expiry_timestamp_seconds`, `agent_account_logged_in` (present and `expiresAt > now`,
  per contract "not known-expired").
- `agent_account_info{plan}` = `unknown`. The plan lives in `.credentials.json.subscriptionType`, which
  `sources.md` does not allow-list. Flag: needs a spec change to read it.

4.2 `claude-transcripts` (default 60 s, floor 30 s):
- walk `<dir>/projects/**/*.jsonl`, per-file byte offset; read only complete lines; size < offset → reread from 0.
- first time a profile is seen (no state): offsets = current sizes, count nothing (contract: no replay).
  Files appearing later start at 0.
- per `type=="assistant"` line: struct-decode only `message.id`, `message.model`, `message.usage.*`, `timestamp`
  (content never decoded into memory structures); skip `<synthetic>`; skip lines whose timestamp is > 24 h old
  (protects against history copied into a new file on resume/fork).
- dedupe by `message.id` within the 24 h horizon: first sight adds usage, a later line with higher numbers adds
  only the positive delta per field (streaming duplicates). Ids older than 24 h pruned.
- emits `agent_tokens_total{type=input|output|cache_read|cache_write}`, `agent_requests_total{outcome="ok"}`
  (one per unique message id). No `reasoning`, no `agent_request_errors_total`, no `agent_cost_usd_total` (the
  transcript has no source for them; absent, not zero).
- model cap: first 20 distinct models per profile keep their name, later ones fold to `other`.
- `provider="anthropic"` fixed.

4.3 `claude-sessions` (15 s): `<dir>/sessions/*.json`, decode `pid`, `status` only; pid alive = `/proc/<pid>`
exists. status `idle`→idle, `busy|running|active|working`→active, else `other`. Emits all three states (0 included)
for each profile with a `sessions/` dir. Real status vocabulary is unverified beyond `idle`: flagged.

4.4 `claude-oauth-usage` (opt-in `quota: {claude_oauth_usage: true}`, 15 m floor, 60 m after 429):
- per call: read `.credentials.json` `accessToken` + `expiresAt` into locals; if `expiresAt <= now` → no request,
  error `auth_expired`. Else `GET https://api.anthropic.com/api/oauth/usage`, headers `Authorization: Bearer …`,
  `anthropic-beta: oauth-2025-04-20`, `User-Agent: agent-quota-exporter/<ver>`. Timeout 10 s. No redirects followed.
  No refresh code exists anywhere.
- 401: `auth_expired`; after 2 consecutive 401s, no request until the credential file's (mtime, expiresAt)
  fingerprint changes. 429: `rate_limited`, backoff 60 m (persisted). 5xx/network: `upstream`/`timeout`.
  Response body never logged; only status class.
- mapping: `five_hour`→`5h/all`, `seven_day`→`7d/all`, `five_hour_X`→`5h/X`, `seven_day_X`→`7d/X`, other
  top-level object with numeric `utilization` → window = key, scope `all`. ratio = utilization/100; `resets_at`
  → reset timestamp (absent if null). `limits[]` with `is_active`: `session`→5h/all, `weekly_all`→7d/all,
  `weekly_X`→7d/X, other kind → window = kind, scope `all`; emits `agent_quota_limit_info{severity}`.
  `agent_quota_observed_timestamp_seconds` on success.
- two opted-in profiles with the same account uuid: query only the first by alias, warn once (no duplicate call).

## 5. Tests (acceptance = spec phase 1 + 2 criteria that apply)

| Test | Proves |
|---|---|
| golden: fixtures → exact `/metrics` text (fixed clock) | contract names/labels/units; renderer |
| two Claude profiles on one host → disjoint, correctly labelled series | multi-profile |
| restart with state → counters continue; state deleted → start at zero, no replay | counter semantics |
| streaming duplicate counts once (412 not 160+412); `<synthetic>` skipped; partial last line not consumed | parser intent |
| canary: content field and access token canaries never in `/metrics`, logs, errors | secrets/content |
| profile dir snapshot (names, sizes, mtimes) identical before/after full run | read-only toward agent state |
| stub server: 1 request per account per 15 m at 1 s scrape rate; 429 → 60 m silence; 2×401 → silence until cred file changes; only `/api/oauth/usage` on the stub host contacted | rate gates, no refresh |
| config: each validation failure in 3.4 fails loudly | config |
| stale quota dropped after `stale_after` | absence ≠ zero |
| fixture secret-scan greps (fixtures/README) empty | repo hygiene |

Not verifiable here (on-prem agent): real `~/.claude` layouts, real session status values, real endpoint ratios vs
the vendor's display, flock and `/proc` on the nodes, systemd unit.

## 6. Delivery

1. PR `feat/phase1-core` → main with ledger updates (WORK_TRACKER Active, SESSION, MANIFEST), `AI-Model:` trailers.
2. Tag `v0.1.0-rc.1` on the PR head → release workflow publishes tarballs + SHA256SUMS for the on-prem test.
3. After the test and Tu's merge: tag `v0.1.0` on main; tell the orchestrator (via Tu) the tag.
4. Then: label map PR in agent-fleet-kit `mesh/`.
