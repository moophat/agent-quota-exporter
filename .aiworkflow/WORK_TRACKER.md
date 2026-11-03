# WORK_TRACKER
**Last-Updated:** 2026-11-03T19:35:00Z

## Active — Exporter v0.1, Claude only | 2026-11-03
<!-- BEGIN STUB: 2026-11-03 | Exporter v0.1, Claude only -->
### Exporter v0.1, Claude only | 2026-11-03
**Meta:** feat/phase1-core | claude-opus-5-5 | `plans/261103-0900-exporter-v0.1-claude/plan.md`
**Model:** claude-opus-5-5
**Outcome:** v0.1.0-rc.1 released 2026-11-03 (tag at c5a6750, prerelease, 2 linux tarballs + SHA256SUMS, checksums verified, released binary passes the cloud end-to-end run); PRs #1-#3 merged after Qodo review and green CI; release by tag push, main-only dispatch or a `release/<tag>` branch push (PR #3, for the cloud lane)
**Key:** Go, one dep (yaml.v3), hand-written exposition; plan red-teamed by 4 reviewers (32 findings adjudicated in plan.md); opencode dropped from this lane (Tu 2026-11-02); quota opt-in, host-wide 429 backoff, never refreshes
**Pending:** on-prem integration test against real profiles (Tu's on-prem agent); Tu: ruleset or protected environment for releases (PR #3 security thread), delete stale branch release/v0.1.0-rc.1 (proxy blocks it here), subscriptionType allow-list, contract rows for state_epoch/process_start, NFS homes; v0.1.0 after the on-prem test
<!-- END STUB: 2026-11-03 | Exporter v0.1, Claude only -->
- [x] Plan, red team, adjudication
- [x] Phases 01-05 (config, core, account/sessions, transcripts, quota)
- [x] Phase 06 acceptance tests, CI, release workflow, examples, README
- [x] PR #1 + Qodo review (3 rounds), merged
- [x] PR #2 (release dispatch, bounded CI), PR #3 (release branch trigger, cloud end-to-end run)
- [x] v0.1.0-rc.1 released and verified; label map in agent-fleet-kit (PR #119, merged)
- [ ] on-prem integration test, v0.1.0

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
