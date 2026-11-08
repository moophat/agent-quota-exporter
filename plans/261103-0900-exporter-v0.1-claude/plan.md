---
title: agent-quota-exporter v0.1 (Claude Code only)
description: First working exporter binary: core, Claude local-file plugins and the opt-in Claude quota plugin, with CI and a tag release.
status: done
priority: P1
effort: 6 phases
branch: feat/phase1-core
tags: [backend, go, prometheus, exporter]
blockedBy: []
blocks: [agent-fleet-kit label map (mesh/), fleet exporter role]
created: 2026-11-03
author: claude-opus-5-5 (cloud lane)
---

# agent-quota-exporter v0.1 — plan

Design rationale and behaviour detail: [design.md](design.md) (section numbers below refer to it).
Authority, in order: `specs/decisions.md` > `specs/metric-contract.md` > `specs/sources.md` > `specs/plan.md`.

## Outcome
One static Go binary per node that exposes, per Claude Code profile (`CLAUDE_CONFIG_DIR`): token and request
counters from transcripts, live session counts, login state and credential expiry, and (opt-in) the vendor's 5h/7d
quota windows, all per contract 0. Released as `v0.1.0` (rc first) with linux amd64/arm64 tarballs and SHA256SUMS.

## Non-goals
opencode, codex, agy, billing plugins, OTLP, per-session metrics, packages/container/signing, any write to agent
state, any token refresh. Graph label map is a separate PR in agent-fleet-kit after this one (Tu, 2026-11-02).

## Phases

| # | Phase | Depends on | Verify |
|---|---|---|---|
| 01 | [Scaffold, config, discovery](phase-01-scaffold-config-discovery.md) | — | `go test ./internal/config/...` |
| 02 | [Core: state, scheduler, renderer, HTTP, CLI](phase-02-core.md) | 01 | `go test ./internal/core/...`, `--once` runs |
| 03 | [Claude account + sessions](phase-03-claude-account-sessions.md) | 02 | `go test ./internal/claude/ -run 'Account\|Sessions'` |
| 04 | [Claude transcripts](phase-04-claude-transcripts.md) | 02 | `go test ./internal/claude/ -run Transcripts` |
| 05 | [Claude OAuth usage (quota)](phase-05-claude-oauth-usage.md) | 02, 03 | `go test ./internal/claude/ -run OAuth` |
| 06 | [Acceptance, CI, release, ledgers, PR](phase-06-acceptance-release.md) | 01–05 | `go vet ./... && go test -race ./...`, fixture greps, workflows lint |

Execution is sequential on one branch (one cohesive codebase; parallel lanes would fight over `internal/core`).

## Global acceptance (all must hold before the PR)
1. `go vet ./... && go test -race -count=1 ./...` green; no skipped test.
2. Golden test: committed fixtures produce byte-exact expected `/metrics` text.
3. Canary test: content and token canaries absent from `/metrics`, logs and errors.
4. Read-only test: profile dir tree (names, sizes, mtimes, modes) unchanged after a full run.
5. Rate-gate tests against a stub server pass (1 req / account / 15 min; 429 → 60 min; 2×401 → stop until change).
6. `fixtures/README.md` secret greps print nothing.
7. Every metric name/label emitted appears in `specs/metric-contract.md` (test greps the contract file).

## Red Team Review

### Session — 2026-11-03
Reviewers: Assumption Destroyer, Failure Mode Analyst, Scope & Contract Critic and Security Adversary (S rows). Adjudicated by the planner without a user round (Tu: "no check-ins, marathon"). **This table overrides
design.md and the phase files wherever they disagree.**

| # | Finding | Sev | Disposition | Applied to |
|---|---|---|---|---|
| R1 | 24 h timestamp gate drops backlog after any outage > 24 h | Crit | Accept: no age filter on lines past a stored offset. The age filter applies only to a file first seen *after* init (read from 0): skip its lines older than that file's first-seen time − 1 h (resume/fork copies). `seen` bounded by count (100k ids, oldest evicted) | ph04 |
| R2 | 64 MiB per-collect budget | Med | Accept: dropped (first run starts at EOF, so no backlog to budget). 16 MiB line cap stays (bufio needs a bound) | ph04 |
| R3 | `--once` ignores the 15 min quota floor; flock clash | High | Accept: `--once` skips `claude-oauth-usage` unless `--once-with-quota`, which still honours persisted gates and persists `last_attempt` | ph02 |
| R4 | Concurrent collects race on one `state.json` | High | Accept: one mutex-guarded state; snapshot + atomic write under the lock (single writer) | ph02 |
| R5 | Timeout/error loses or half-applies plugin progress | High | Accept: `Collect` returns `Result{Samples, Errors map[reason]int}` + error; plugin mutates its state only for work it finished; transcripts honours ctx and returns partial success with progress saved; non-fatal parse errors counted, not fatal | ph02, ph04 |
| R6 | Empty/unknown uuid dedupes quota away; uuid only read at start | Crit | Accept: no construction-time dedupe. Every opted-in profile has its own source. A shared in-memory cache keyed by non-empty account uuid: if another profile fetched the same account within `min_interval`, reuse its parsed result (one vendor call per account). Empty uuid never shares | ph05 |
| R7 | Re-login / late account resolution moves totals across accounts | High | Accept: transcript totals keyed by (account, model) at count time; account resolved synchronously before the first transcripts collect | ph03, ph04 |
| R8 | Quota absent after every restart | High | Accept: last good quota samples + `observed_at` persisted in plugin state and served after restart until stale. The 429 gap (stale 45 m < backoff 60 m) is per contract; documented | ph05 |
| R9 | Corrupt state resets the 429/401 gates | High | Accept: on quarantine, quota sources start with a 60 m backoff | ph02 |
| R10 | `state_epoch` / `process_start_time_seconds` not in contract tables | High | Accept (no spec edit, AGENTS.md: specs read-only unless asked): contract test parses the tables (names + label sets); those two are allow-listed as "named in contract prose (Counter semantics)"; spec gap flagged to Tu | ph06 |
| R11 | Model cap "first 20" ≠ contract "top 20 by recent use"; config key unused | Med | Accept: cap from `metrics.max_models_per_profile`, ranked by last-seen at emit time; caveat: a model leaving the top N folds into `other` (that series can then decrease = counter reset), documented | ph04 |
| R12 | Shared freshness bound | Med | Effective = max(`defaults.stale_after`, 2*P + D + 15s); P is effective poll interval, D collection timeout; any positive configured floor | ph02 |
| R13 | Quota mapping emits non-contract windows (`extra_usage`), nulls, duplicate `limit_info` | High | Accept: only `five_hour*` / `seven_day*` keys; null object or null utilization → absent; other keys ignored; `limit_info` deduped by label set; fixture cases added | ph05 |
| R14 | Spec ph2 "only usage host+path contacted" weakened | High | Accept: test `RoundTripper` fails the test on any other host/path; heap-dump canary descoped (flagged) | ph05 |
| R15 | Read-only criterion replaced by a snapshot | Med | Accept: test chmods the profile tree 0555/0444 (enforcing when CI runs non-root) *and* compares snapshots | ph06 |
| R16 | Golden `--once` cannot be deterministic | Crit | Accept: core takes an injected clock (also used for read duration); golden test runs in-process: collect (init) → append fixture → collect; `version="test"` | ph02, ph06 |
| R17 | `expiresAt` unit unstated | Med | Accept: one parser, ms → s explicit, fixture with known ms value | ph03, ph05 |
| R18 | `claude-account` described two ways | Med | Accept: it is a source (60 s, gets health series, re-reads only on mtime change) writing a mutex-guarded `Account` on the profile | ph03 |
| R19 | Session liveness ignores pid reuse (`procStart`), hidepid | Med | Partial: `/proc/<pid>` existence kept (procStart format unverified); flagged for on-prem check | ph03 |
| R20 | NFS-shared homes double count | High | Reject for v0.1: fleet homes are node-local (`/root`); NFS is `/mnt/shared`, outside discovery roots. Documented as an operator rule | — |
| R21 | `agent_account_info{plan="unknown"}` carries nothing | Med | Keep (contract-conformant, cheap); question to Tu: allow-list `subscriptionType` in sources.md? | — |
| R22 | Hand-rolled exposition vs client_golang | Med | Reject: one dep (yaml) and exact golden output; plan.md lists client_golang as an option, not a rule | — |
| R23 | Root default in systemd unit vs spec "dedicated user" | Med | Accept as flagged deviation: example unit runs as root with read-only protections, comment explains the dedicated-user alternative | ph06 |
| R24 | Private-repo release download needs auth | Med | Note: on-prem fetch with `gh release download` using Tu's gh login; no new secret | ph06 |
| S1 | `--once` bypasses the quota floor | Crit | Accept: same fix as R3 | ph02 |
| S2 | Gates per profile, not per host IP; `last_attempt` persisted after the request | High | Accept: a 429 on any quota source backs off **all** quota sources on the host; quota requests spaced ≥ 30 s apart host-wide; `last_attempt` persisted (fsync) *before* a network call. Alias-rename resetting a gate: reject (rare, operator action) | ph02, ph05 |
| S3 | Root reads user-controlled files: FIFO hangs a read slot, symlinked credentials | High | Accept: every file open is `O_RDONLY|O_NOFOLLOW|O_NONBLOCK` + `fstat` regular-file check, then a size cap. Ownership checks: reject for v0.1 (fleet homes are root's) | ph03–05 |
| S4 | `claude-account` copies the access token every 60 s | High | Accept: presence decoded by a custom `UnmarshalJSON` that records only a bool; only the quota plugin ever holds the token. Heap-dump canary: descoped, flagged (Go cannot guarantee zeroing) | ph03 |
| S5 | Paths / session ids in logs and state | Med | Accept: errors map to the bounded reason set and logs carry source/profile/reason only; state keys file paths and message ids by sha256 prefix; state dir forced 0700, files 0600 on every start | ph02, ph04 |
| S6 | `model` label unbounded | Med | Accept: lowercase, must match `^[a-z0-9._:/@\[\]-]{1,64}$`, else `other` | ph04 |
| S7 | Malformed token can reach the endpoint; env proxy; 401≡403 | Med | Accept: token shape check (printable ASCII, no whitespace, 20–8192 bytes); re-read once if mtime changed during the read; no environment proxy (direct only); 403 → `upstream`, not counted as 401 | ph05 |
| S8 | Release supply chain | Med | Accept: actions pinned by commit SHA; tag passed via `env:` and validated `^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$`; rc tags are marked prerelease and are for the on-prem test only, final tags from merged `main`. Provenance attestation: deferred (phase 5) | ph06 |

### Whole-Plan Consistency Sweep
Superseded statements, now void where they appear: design §4.2 "skip lines whose timestamp is > 24 h old" and
"first 20 distinct models" (R1, R11); phase-04 "Budget 64 MiB" (R2) and `totals: {model: …}` (R7: `{account|model}`);
phase-02 `--once` "ignoring min_interval" (R3) and `Collect(...) ([]Sample, error)` (R5); phase-05 step 6
construction-time dedupe (R6); design §3.2 flat 45 m (R12); phase-06 "`--once` prints the golden output" (R16).
Also void: phase-03 `accessToken json.RawMessage` (S4); phase-05 "Transport: proxy from env" and 401/403 equivalence (S7); phase-02 gate keyed only per profile for 429 (S2).
No unresolved contradiction remains.

### Open questions for Tu (do not block v0.1) — 1 and 2 answered in D5; 3 answered in part in D6 (the NFS server node not yet inspected; `specs/decisions.md`)
1. Allow-list `.credentials.json` `subscriptionType` so `agent_account_info{plan}` is real? (R21)
2. Add `agent_quota_exporter_state_epoch` and `process_start_time_seconds` rows to the contract health table? (R10)
3. Are any fleet `CLAUDE_CONFIG_DIR`s on NFS? (R20)
