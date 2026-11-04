# WORK_TRACKER
**Last-Updated:** 2026-11-04T16:40:00Z

## Active — Claude quota: unprefixed windows and limit percents | 2026-11-04
<!-- BEGIN STUB: 2026-11-04 | Claude quota: unprefixed windows and limit percents -->
### Claude quota: unprefixed windows and limit percents | 2026-11-04
**Meta:** fix/claude-other-windows | opus | issue #16
**Model:** opus
**Outcome:** in progress: parser, fixture, tests and spec done; PR, review, release pending
**Key:** unknown top-level window → window="other", scope=key, max 8 per response by key name, no length guessed; limits[].percent → ratio of its window/scope whether active or not, top-level wins; extra_usage = paid allowance past the limits (credits), ratio only
**Pending:** PR into main, Qodo, merge, release with #14 fixes, comment and close #16
<!-- END STUB: 2026-11-04 | Claude quota: unprefixed windows and limit percents -->
- [x] Parser, fixture, tests (4 mutants caught), spec and contract note
- [ ] PR, review, merge, release, issue comment

## Paused
None

## Log

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
