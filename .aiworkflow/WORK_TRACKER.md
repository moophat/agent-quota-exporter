# WORK_TRACKER
**Last-Updated:** 2026-11-07T00:00:00Z

## Paused
None

## Active — Q27 identity history repairs | 2026-11-07
<!-- BEGIN STUB: 2026-11-07 | Q27 identity history repairs -->
### Q27 identity history repairs | 2026-11-07
**Meta:** feat/identity-history | t9r/cx/gpt-5.6-terra | `/root/orchestrator/plans/261106-2205-usage-parity/q27-brief.md`
**Model:** t9r/cx/gpt-5.6-terra
**Outcome:** all five Qodo findings fixed; requested tests, vet, formatting, diff check and focused race checks pass
**Key:** UUID-safe quota migration, optional fallback merge, durable identity replay, 128-sample no-store cap, one-envelope retry buffer
**Pending:** no commit or push
<!-- END STUB: 2026-11-07 | Q27 identity history repairs -->
- [x] Establish baseline and inspect code
- [x] Repair confirmed findings with regressions
- [x] Run requested verification

## Log

<!-- BEGIN STUB: 2026-11-07 | B04 identity history -->
### B04 identity history | 2026-11-07
**Meta:** feat/identity-history | t9r/cx/gpt-6.1-sol | `/root/orchestrator/plans/261106-2205-usage-parity/b04-brief.md`
**Model:** t9r/cx/gpt-6.1-sol
**Outcome:** B04 implemented; identity tests, affected packages, vet, race and formatting checks pass
**Key:** exact allowlist; keyed email fallback; sampled switch gaps ambiguous
**Pending:** none; no commit or push; minimal app wiring required for B03 store/key
<!-- END STUB: 2026-11-07 | B04 identity history -->

<!-- BEGIN STUB: 2026-11-07 | Q26 sink metrics repairs -->
### Q26 sink metrics repairs | 2026-11-07
**Meta:** feat/event-sink-config | t9r/cx/gpt-6.1-sol | `/root/orchestrator/plans/261106-2205-usage-parity/q26-brief.md`
**Model:** t9r/cx/gpt-6.1-sol
**Outcome:** All three findings fixed; regressions failed before fixes; acceptance tests, vet, formatting and focused race checks pass
**Key:** per-writer durability; safe rejected identity; capacity I/O outside append mutex
**Pending:** none; no commit or push
<!-- END STUB: 2026-11-07 | Q26 sink metrics repairs -->

<!-- BEGIN STUB: 2026-11-06 | B03 event sink configuration -->
### B03 event sink configuration | 2026-11-06
**Meta:** feat/event-sink-config | t9r/cx/gpt-6.1-sol | `/root/orchestrator/plans/261106-2205-usage-parity/b03-brief.md`
**Model:** t9r/cx/gpt-6.1-sol
**Outcome:** B03 implemented; focused tests, CLI startup tests, vet, race and formatting checks pass
**Key:** disabled compatibility; explicit byte budget; no source event writes
**Pending:** full suite fails agy TestClientRunsAsHomeOwner/65534 permission check; no agy edits; no commit or push
<!-- END STUB: 2026-11-06 | B03 event sink configuration -->

<!-- BEGIN STUB: 2026-11-06 | Q25 event sink repairs -->
### Q25 event sink repairs | 2026-11-06
**Meta:** feat/event-sink | t9r/cx/gpt-6.1-sol | `/root/orchestrator/plans/261106-2205-usage-parity/q25-brief.md`
**Model:** t9r/cx/gpt-6.1-sol
**Outcome:** Ten findings repaired; focused tests, vet, formatting and diff checks pass; regressions exercised against original implementation
**Key:** rebuild compact hash index once on open; reconcile pending durability before checkpoints; strict recursive allowlists
**Pending:** no requested acceptance checks pending; no commit or push
<!-- END STUB: 2026-11-06 | Q25 event sink repairs -->

<!-- BEGIN STUB: 2026-11-06 | B01 Qodo findings -->
### B01 Qodo findings | 2026-11-06
**Meta:** feat/usage-parity-events | t9r/cx/gpt-6.1-sol | `specs/event-contract.md`
**Model:** t9r/cx/gpt-6.1-sol
**Outcome:** Five findings fixed; focused consistency checks and git diff --check pass; original decision entries byte-for-byte unchanged
**Key:** canonical freshness margin; bounded hot store with verified archive; no inferred owner rulings; keyed identities
**Pending:** Runtime implementation not in scope; maintainer question on D4/private events remains open
<!-- END STUB: 2026-11-06 | B01 Qodo findings -->

<!-- BEGIN STUB: 2026-11-06 | B02 durable event sink -->
### B02 durable event sink | 2026-11-06
**Meta:** feat/event-sink | t9r/cx/gpt-5.6-terra | `/root/orchestrator/plans/261106-2205-usage-parity/b02-brief.md`
**Model:** t9r/cx/gpt-5.6-terra
**Outcome:** B02 complete; focused tests, vet, format, diff check pass
**Key:** JSONL monthly shards; cursor callback after shard and directory fsync; idempotent event IDs; budget and reserve preflight
**Pending:** no B02 items; B01 permission prose (0640/0750) conflicts with B02 brief (0600/0700), followed B02 brief
<!-- END STUB: 2026-11-06 | B02 durable event sink -->

<!-- BEGIN STUB: 2026-11-06 | B01 durable event contract -->
### B01 durable event contract | 2026-11-06
**Meta:** feat/usage-parity-events | t9r/cx/gpt-6.1-sol | `specs/event-contract.md`
**Model:** t9r/cx/gpt-6.1-sol
**Outcome:** B01 contract complete; 17/17 L1 coverage verified; original decisions preserved; documentation checks pass
**Key:** B01 only; optional generic events; public metric dimensions bounded; Tu rulings preserved
**Pending:** agy refresh/retained-reader authority conflict; Codex response and manual charge units remain unverified; no runtime work
<!-- END STUB: 2026-11-06 | B01 durable event contract -->

<!-- BEGIN STUB: 2026-11-05 | PR #21 cache trust and health fixes -->
### PR #21 cache trust and health fixes | 2026-11-05
**Meta:** feat/claude-statusline-cache | codex | PR #21
**Model:** codex
**Outcome:** complete in working tree; focused regressions, go vet ./... and go test -race ./... exit 0; diff reviewed
**Key:** synthetic files only; no live machine, credential contents, claude, commits, push or tags
**Pending:** live cache/systemd and non-root execution unverified; existing SQLite root-only skip and helper skip; no commit, push or tag
<!-- END STUB: 2026-11-05 | PR #21 cache trust and health fixes -->

<!-- BEGIN STUB: 2026-11-05 | Claude statusline cache (#20) -->
### Claude statusline cache (#20) | 2026-11-05
**Meta:** feat/claude-statusline-cache | codex | issue #20
**Model:** codex
**Outcome:** complete in working tree; focused tests, go vet ./... and go test -race ./... pass; reviewed and cadence fix verified
**Key:** default root profile only; shared parser; persisted vendor gates unchanged; no live-node access or release
**Pending:** live-node age and systemd runtime unverified by instruction; no commit, push or tag
<!-- END STUB: 2026-11-05 | Claude statusline cache (#20) -->

<!-- BEGIN STUB: 2026-11-04 | Claude quota: unprefixed windows and limit percents -->
### Claude quota: unprefixed windows and limit percents | 2026-11-04
**Meta:** fix/claude-other-windows | opus | issue #16
**Model:** opus
**Outcome:** merged (PR #17, 4ddde41); v0.2.0-rc.3 released from 4ddde41 with #14 fixes and verified (checksums, version, Claude itest); #14 and #16 commented and closed. Qodo: 2 findings (scope collisions) fixed
**Key:** unknown top-level window → window="other", scope=key, max 8 per response by key name, no length guessed; limits[].percent → ratio of its window/scope whether active or not, top-level wins; extra_usage = paid allowance past the limits (credits), ratio only
**Pending:** on-prem retest of rc.3 (`plans/handoffs/v0.2-rc-test-20261104.md`), then v0.2.0
<!-- END STUB: 2026-11-04 | Claude quota: unprefixed windows and limit percents -->

<!-- BEGIN STUB: 2026-11-04 | v0.2 rc.2 fixes: Codex model label, router provider labels -->
### v0.2 rc.2 fixes: Codex model label, router provider labels | 2026-11-04
**Meta:** fix/v0.2-labels | opus | issue #14, `plans/handoffs/v0.2-rc-test-result-20261104.md`
**Model:** opus
**Outcome:** merged (PR #15, e2333ec): Codex model search before the start offset, retried after a failed read; router provider prefix plus hash; docs. Qodo findings fixed; tests pass as root and non-root
**Key:** Codex file started at its end takes the model of its last turn_context before that offset (backward search, 16 MiB cap); router provider id over 48 chars keeps prefix minus uuid plus 8-hex hash; state saved per source kept (durability); agy unit needs PATH, ReadWritePaths for its HOME, MemoryMax 512M
**Pending:** v0.2.0-rc.3 with #16, comment and close #14
<!-- END STUB: 2026-11-04 | v0.2 rc.2 fixes: Codex model label, router provider labels -->

<!-- BEGIN STUB: 2026-11-04 | Exporter v0.2, more sources -->
### Exporter v0.2, more sources | 2026-11-04
**Meta:** feat/v0.2-sources | claude-opus-5-5 | `plans/261104-1800-more-sources/plan.md`
**Model:** claude-opus-5-5
**Outcome:** v0.2 merged (PR #10, 14aeb20); v0.2.0-rc.2 released from 75697f0 and verified (checksums, Claude itest); codex-rollouts, opencode-db, router-9router, agy-usage with sqlro and safefs; red-team, code review and Qodo findings fixed
**Key:** passive quota served until its own reset; credits 0 is not "out"; one Codex reading per scope; WAL DB without sidecars skipped so nothing is created in agent dirs; agy disables itself if /usage spends a turn; provider balances deferred
**Pending:** on-prem test of v0.2.0-rc.2 (`plans/handoffs/v0.2-rc-test-20261104.md`), then v0.2.0. Ruled 2026-11-04: -shm read-marks accepted (D8), balances not built (D9), idle ~16 MB accepted (D10)
<!-- END STUB: 2026-11-04 | Exporter v0.2, more sources -->

<!-- BEGIN STUB: 2026-11-03 | Exporter v0.1, Claude only -->
### Exporter v0.1, Claude only | 2026-11-03
**Meta:** feat/phase1-core | claude-opus-5-5 | `plans/261103-0900-exporter-v0.1-claude/plan.md`
**Model:** claude-opus-5-5
**Outcome:** v0.1.0 released 2026-11-04 (tag at c6f4dc8, cut on-prem) after the real-node test of rc.2: counters equal an independent transcript recount, quota matches the fleet collector, strace shows no writes outside the state dir; PRs #1-#7 merged
**Key:** Go, one dep (yaml.v3), hand-written exposition; plan red-teamed by 4 reviewers (32 findings adjudicated in plan.md); opencode dropped from this lane (Tu 2026-11-02); quota opt-in, host-wide 429 backoff, never refreshes
**Pending:** fixes as v0.1.x only; `shell` session status meaning unknown (counted as other); untested: arm64 binary, systemd runtime, VictoriaMetrics ingestion; stale remote branches (Tu); fleet rollout is on-prem (FleetManager01), not this lane
<!-- END STUB: 2026-11-03 | Exporter v0.1, Claude only -->

<!-- BEGIN STUB: 2026-10-28 | Repo convention setup -->
### Repo convention setup | 2026-10-28
**Meta:** main | claude-sonnet-5-5 | `specs/plan.md`
**Outcome:** repo restructured to Tu's convention: AGENTS.md (rule block) with CLAUDE.md symlink, specs/, ledgers plus rotation script, secret-scanning guards; licence locked Apache-2.0; no code, not committed
**Key:** specs/ (git mv from docs/), specs/decisions.md (D1 locked; D2 language, D3 public timing, D4 per-session open), rotate-work-tracker.py copied unchanged, secrets CI on ubuntu-latest
**Pending:** orchestrator commits signed; D2-D4 open; no gitleaks binary on node, so fixtures not scanned by it
<!-- END STUB: 2026-10-28 | Repo convention setup -->

(Earlier entry, 2026-10-28 03:38 +07, claude-sonnet-5-5: initial repo setup, docs/plan, contract, sources, fixtures, no code.)
