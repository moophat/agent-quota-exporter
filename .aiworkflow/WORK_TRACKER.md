# WORK_TRACKER
**Last-Updated:** 2026-11-04T02:55:00Z

## Active — Exporter v0.1, Claude only | 2026-11-03
<!-- BEGIN STUB: 2026-11-03 | Exporter v0.1, Claude only -->
### Exporter v0.1, Claude only | 2026-11-03
**Meta:** feat/phase1-core | claude-opus-5-5 | `plans/261103-0900-exporter-v0.1-claude/plan.md`
**Model:** claude-opus-5-5
**Outcome:** v0.1.0 released 2026-11-04 (tag at c6f4dc8, cut on-prem) after the real-node test of rc.2: counters equal an independent transcript recount, quota matches the fleet collector, strace shows no writes outside the state dir; PRs #1-#7 merged
**Key:** Go, one dep (yaml.v3), hand-written exposition; plan red-teamed by 4 reviewers (32 findings adjudicated in plan.md); opencode dropped from this lane (Tu 2026-11-02); quota opt-in, host-wide 429 backoff, never refreshes
**Pending:** fixes as v0.1.x only; `shell` session status meaning unknown (counted as other); untested: arm64 binary, systemd runtime, VictoriaMetrics ingestion; stale remote branches (Tu); fleet rollout is on-prem (FleetManager01), not this lane
<!-- END STUB: 2026-11-03 | Exporter v0.1, Claude only -->
- [x] Plan, red team, adjudication
- [x] Phases 01-05 (config, core, account/sessions, transcripts, quota)
- [x] Phase 06 acceptance tests, CI, release workflow, examples, README
- [x] PR #1 + Qodo review (3 rounds), merged
- [x] PR #2 (release dispatch, bounded CI), PR #3 (release branch trigger, cloud end-to-end run)
- [x] v0.1.0-rc.1 released and verified; label map in agent-fleet-kit (PR #119, merged)
- [x] PR #5 (bounded plan label, contract rows, D5/D6, on-prem handoff); v0.1.0-rc.2 released and verified
- [x] on-prem integration test (rc.2, on-prem lane), PR #7 (--once-with-quota guard), v0.1.0 released on-prem

## Paused
None

## Log

<!-- BEGIN STUB: 2026-10-28 | Repo convention setup -->
### Repo convention setup | 2026-10-28
**Meta:** main | claude-sonnet-5-5 | `specs/plan.md`
**Outcome:** repo restructured to Tu's convention: AGENTS.md (rule block) with CLAUDE.md symlink, specs/, ledgers plus rotation script, secret-scanning guards; licence locked Apache-2.0; no code, not committed
**Key:** specs/ (git mv from docs/), specs/decisions.md (D1 locked; D2 language, D3 public timing, D4 per-session open), rotate-work-tracker.py copied unchanged, secrets CI on ubuntu-latest
**Pending:** orchestrator commits signed; D2-D4 open; no gitleaks binary on node, so fixtures not scanned by it
<!-- END STUB: 2026-10-28 | Repo convention setup -->

(Earlier entry, 2026-10-28 03:38 +07, claude-sonnet-5-5: initial repo setup, docs/plan, contract, sources, fixtures, no code.)
