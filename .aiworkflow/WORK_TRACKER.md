# WORK_TRACKER
**Last-Updated:** 2026-11-03T13:43:50Z

## Active — Exporter v0.1, Claude only | 2026-11-03
<!-- BEGIN STUB: 2026-11-03 | Exporter v0.1, Claude only -->
### Exporter v0.1, Claude only | 2026-11-03
**Meta:** feat/phase1-core | claude-opus-5-5 | `plans/261103-0900-exporter-v0.1-claude/plan.md`
**Model:** claude-opus-5-5
**Outcome:** PR #1 merged 2026-11-03 (411eb6b) on Tu's go after clean Qodo review and green CI; 13+1 Qodo findings fixed; release via tag push or main-only dispatch (PR #2); first release v0.1.0-rc.1 for the on-prem integration test
**Key:** Go, one dep (yaml.v3), hand-written exposition; plan red-teamed by 4 reviewers (32 findings adjudicated in plan.md); opencode dropped from this lane (Tu 2026-11-02); quota opt-in, host-wide 429 backoff, never refreshes
**Pending:** on-prem integration test against real profiles (Tu's on-prem agent); Tu: subscriptionType allow-list, contract rows for state_epoch/process_start, NFS homes; label map PR in agent-fleet-kit after release
<!-- END STUB: 2026-11-03 | Exporter v0.1, Claude only -->
- [x] Plan, red team, adjudication
- [x] Phases 01-05 (config, core, account/sessions, transcripts, quota)
- [x] Phase 06 acceptance tests, CI, release workflow, examples, README
- [x] PR #1 + Qodo review (3 rounds), merged
- [ ] PR #2 (release dispatch, bounded CI), v0.1.0-rc.1, on-prem integration test, v0.1.0

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
