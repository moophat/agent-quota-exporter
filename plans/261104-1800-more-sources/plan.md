---
title: agent-quota-exporter v0.2 (Codex, opencode, router, provider balances)
description: Quota and usage for what isn't Claude, read passively from local state first; the build plan for the cloud lane to follow.
status: in-progress
priority: P1
branch: plan/phase3-more-sources
tags: [backend, go, prometheus, exporter]
created: 2026-11-04
author: claude-opus-5-5 (on-prem lane), at the maintainer's request
---

# Plan: more sources

The maintainer, 2026-11-04: "Plan first. For it to follow the plan." The cloud lane builds this. It has autonomy over how,
and should change the plan where the code proves it wrong, saying so in the PR. The on-prem lane tests and rolls out.

## Why

v0.1 reads Claude Code only, so an operator with Codex, agy, opencode and a router sees quota for one vendor and nothing
for the rest. `specs/sources.md` and `specs/plan.md` planned this as phases 3 and 4. An on-prem reading on 2026-11-04 changed
the plan in one important way, below.

## Principle: the vendor's own client, passive first

Read what the vendor's own client already wrote to disk. Failing that, ask the vendor's own client, the way a person would.
The new sources hold no token, make no vendor call of their own, and impersonate nothing. Anything active is opt-in and gated.
`claude-oauth-usage` from v0.1 differs: it reads the profile's current token and calls a private endpoint, opt-in. It stays as it is.

## What the on-prem reading found (2026-11-04, Codex CLI 0.163.0)

Shapes are verified on a real node. Fixtures with invented values are in `fixtures/`; add to them as needed.

1. **Codex rollouts already carry the quota.** Every `token_count` event in
   `<CODEX_HOME>/sessions/YYYY/MM/DD/rollout-*.jsonl` has `payload.rate_limits`: `primary` and `secondary`, each with
   `used_percent`, `window_minutes`, `resets_at`; `credits` with `has_credits`, `unlimited`, `balance`; and `limit_id`.
   `payload.info.total_token_usage` and `last_token_usage` carry `input_tokens`, `cached_input_tokens`,
   `cache_write_input_tokens`, `output_tokens`, `reasoning_output_tokens`, `total_tokens`. The model comes from the
   session's turn context. **This replaces `codex app-server` as the first source:** nothing is spawned, so the refresh
   question in `specs/sources.md` doesn't arise for it.
2. **The limit:** the value is as of the profile's last Codex turn, not now. An idle profile has an old reading. That's
   honest if it's labelled, and see the staleness decision below.
3. `state_*.sqlite` table `threads` has `tokens_used`, `model`, `model_provider`, `created_at_ms`, `updated_at_ms`,
   `archived`. It also holds `first_user_message`, `title`, `preview`, `cwd`, `git_*`: **never select those.**
4. **opencode:** `<data>/opencode/opencode.db`, table `message`, column `data` is JSON: `role`, `providerID`, `modelID`,
   `cost`, `tokens{total,input,output,reasoning,cache}`, `time`. Several instances per node, each with its own data dir.
   Matches `specs/sources.md`, including its never-read list.
5. **Router (9router):** `<home>/.9router/db/data.sqlite`. `usageHistory`: `id`, `timestamp`, `provider`, `model`,
   `connectionId`, `promptTokens`, `completionTokens`, `cost`, `status`, `tokens`, `meta`, plus `apiKey`, which is secret.
   `providerConnections`: `id`, `provider`, `authType`, `name`, `email`, `priority`, `isActive`, and `data`, a JSON column
   holding secrets (`accessToken`, `refreshToken`, `idToken`, `apiKey`) next to health fields (`testStatus`, `errorCode`,
   `rateLimitedUntil`, `lastErrorAt`). **Read `data` only through `json_extract` of the health fields; never select the
   column.** The file was 927 MB on the node read: open read-only, read `usageHistory` incrementally by `id`, no full scans.
6. **agy has a real quota surface, which corrects `specs/sources.md`.** The official client answers
   `agy -p /usage --output-format json` with `command.data.groups[].buckets[]`: `id`, `window`, `remaining_fraction`,
   `reset_time`. It used 0 tokens and no model turn (agy CLI 1.5.0, one account, read twice). Local state has no quota. See "agy" below.

## Scope, in order

Each is shippable alone. Stop and release at any point.

1. **`codex-rollouts`** (harness `codex`, provider `openai`, on by default): quota and tokens from the newest rollouts,
   incremental with the existing state store, no replay of history without `backfill_days`. Normalise `window_minutes` to
   `window` (300 to `5h`, 10080 to `7d`). `limit_id` goes to `scope`. Also session counts.
   Discovery: `~/.codex` and any `CODEX_HOME` in config. Multi-account layouts need `profiles:` in config; don't guess at a
   directory convention.
2. **Credits:** a new gauge for `credits.has_credits` and `balance`, since a workspace can run out of credits with windows
   still low. That's the failure operators miss. Name it in the contract.
3. **`opencode-db`:** per `specs/sources.md`. Tokens, cost as reported, model, provider, errors.
4. **`router-9router`:** per-connection requests and tokens from `usageHistory`, and connection health as new gauges
   (`agent_router_connection_up`, `agent_router_rate_limited_until_timestamp_seconds`). Label by connection id or alias,
   hashing an email by the existing account rule. Off by default; it's a named product, so enabling it is a choice.
5. **Provider balances:** the plugin in `specs/sources.md`: GET plus JSON path, keys via `env:`/`file:`. Configurable per
   provider, so it covers an account-usage endpoint as well as the OpenRouter, DeepSeek and GLM kind.
6. **`agy-usage`:** see "agy". Worth doing early if the build goes well: it's small, and an operator with many agy accounts
   has no other way to see them.
7. **`codex-rate-limits` (active):** only if the passive reading proves too stale in use. Off by default, and needs the
   refresh measurement `specs/plan.md` asks for, which is on-prem's to run.

## Decisions

- **Staleness of passive quota.** The contract drops a quota value past `stale_after`. For a passive reading that would hide
  a true number on any idle profile. Decision: a passive quota series stays until its own `resets_at` passes, then goes
  absent; `agent_quota_observed_timestamp_seconds` carries its true age so the consumer can say "as of". This is a deliberate
  exception to the contract's staleness rules, so the same PR changes them to name it: passive sources only, per source, in
  `specs/metric-contract.md`. Live sources keep `stale_after`. Series and types don't change, so contract stays 0.
- **Contract stays 0** if everything is additive. If it isn't, bump it and say why.
- **Fixed vocabulary, bounded cardinality:** same rules. No per-session labels.
- **No fleet data in this repo:** names, addresses and mappings stay on the on-prem side.
- **Release:** `v0.2.0-rc.N`, cut when scope items 1 to 3 pass; the on-prem lane tests each rc on real nodes before `v0.2.0`.

## agy

`agy-usage` (harness `agy`, provider `google`, opt-in): run the official client per profile, `HOME=<profile> agy -p /usage
--output-format json`, with no shell, a timeout, and a fixed argv. Parse `command.data.groups[].buckets[]`:

- `agent_quota_utilization_ratio` = `1 - remaining_fraction`.
- `window` from the bucket's `window` (`weekly`, `5h`). `scope` from the bucket id's group: `gemini` and `3p`.
- `agent_quota_reset_timestamp_seconds` from `reset_time`.
- Every other field is ignored. Bucket ids outside the fixed vocabulary go to `scope="other"` and a health counter, so a vendor can't grow label values.
- Default interval 30 minutes, floor 15. Normal staleness rules apply, since this is a live reading.
- Serialise calls across profiles, and back off on any non-zero exit.
- One account may be logged in on several machines, so config must let an operator read each account from one host only.

Why not the alternatives. CLIProxyAPI's management UI and cockpit-tools both reach the same data by calling Google's private
`retrieveUserQuotaSummary` themselves, with their own identity headers and tokens they hold; the agy binary contains that
same call, so `/usage` is the client making it.

Add this to `specs/sources.md`, replacing "no quota surface". It ships off by default. Measured on-prem on 2026-11-04, one account, by hash and mtime only: a call
with an expired access token refreshes it and rewrites the token file, as any agy run does; two calls with a live token changed
nothing. So each poll can trigger the client's normal refresh, and the docs must say so. Whether the refresh token itself rotates
wasn't captured at that call; earlier on-prem testing found it doesn't. On-prem repeats that check on an rc before `v0.2.0`, and
if it rotates, the source stays out of the release. Fixture: `fixtures/agy/usage-command.json`.

## Acceptance

Same bar as v0.1.

- Each source has a golden-metrics test against a fixture, and a test that the secret columns and files are never read
  (the query allow-list, as in `specs/sources.md`). The tests are per table: selecting `threads.first_user_message`, `providerConnections.data` as a whole or `usageHistory.apiKey` must fail; `json_extract` of listed `message.data` paths and of the listed health fields are allowed; `message.data` whole is not (`specs/sources.md` wins, design verdict 9).
- Missing or unreadable state gives `agent_source_up 0`, never a zero value.
- `strace` on a real node, on-prem: no writes outside the state dir, and no network from local-state plugins.
- Counters equal an independent recount on a real node, as v0.1 did for Claude.
- Idle cost: under 15 MB and negligible CPU with a large router DB and a dozen profiles.

## On-prem (not the cloud lane's)

- Test each rc on real nodes and write results to `plans/handoffs/`.
- Fleet side, in its own repo: profile config for every account, label map, scrape rules, the page's Usage screen.

## Claude stays on polling

The maintainer, 2026-11-04: "Ok stay polling for cc then". `claude -p /usage` was tried on-prem: it works with 0 tokens, but returns
English text with fewer windows and less precision. `claude-oauth-usage` stays the source for Claude.

## Out of scope

Anything that refreshes a token itself, calls a vendor's private endpoint itself, or reads message text. The one spawned client
is `agy-usage` above, opt-in. Any change to how v0.1 reads
Claude. A selection or recommendation feature: this tool reports, it never chooses.
