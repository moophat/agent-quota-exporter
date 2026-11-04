# MANIFEST: agent-quota-exporter
**Last-Updated:** 2026-11-04T14:20:00Z

Status values: planned, in-progress, done. Updated 2026-11-04 by claude-opus-5-5. Licence Apache-2.0 (locked). Language Go (locked). v0.1 (Claude only) merged to `main` (PR #1, 2026-11-03); release **v0.1.0** published 2026-11-04 (tag at c6f4dc8, cut on-prem after the real-node test of rc.2); fixes ship as v0.1.x. Release by tag push, main-only dispatch, or a `release/<tag>` branch push. Tests: all passing (`go test -race ./...`). Contract 0, validated by the golden and contract-parser tests.

| Component | Status | Notes |
|---|---|---|
| Spec: plan, metric contract, sources, decisions | done (draft v0) | design phase; `specs/` |
| Repo guards: gitleaks config, pre-commit, secrets workflow | done | copied from the TSB repos |
| Fixtures | done (synthetic) | see `fixtures/README.md` |
| Core: config loader and validation | done | `internal/config`, feat/phase1-core |
| Core: profile discovery | done (Claude) | `internal/config/discover.go` |
| Core: scheduler, cache, rate gates, state store | done | `internal/core` |
| Core: `/metrics`, `/healthz`, health metrics | done | `internal/core/render.go`, `http.go` |
| Plugin: claude-transcripts, claude-sessions, claude-account | done, verified on-prem (v0.1.0-rc.2) | `internal/claude` |
| Plugin: opencode-db | done (v0.2, merged; rc.2), awaiting on-prem test | `internal/opencode`; opencode-http not built |
| Plugin: claude-oauth-usage (quota) | done, verified on-prem (v0.1.0-rc.2) | opt-in; `internal/claude/oauthusage.go` |
| Plugin: codex-rollouts (quota, credits, tokens) | done (v0.2, merged; rc.2), awaiting on-prem test | `internal/codex`; codex-state and codex-rate-limits not built |
| Plugin: agy-usage (opt-in) | done (v0.2, merged; rc.2), awaiting on-prem test | `internal/agy`; runs the client's own /usage |
| Plugin: router-9router | done (v0.2, merged; rc.2), awaiting on-prem test | `internal/router`; declared profiles only |
| Provider plugins: Anthropic/OpenAI admin usage | planned | balances not built (D9) |
| OTLP output | planned | phase 4 |
| Packaging: binary/package, systemd unit, container | planned | phase 5 |
| Example dashboards and alert rules | planned | phase 5 |
| CI: tests, golden metrics, secret-scan of fixtures | done | `.github/workflows/ci.yml`, `release.yml` |

## Known issues / deviations (v0.1)
- Release triggers have no repository-side control (no ruleset on `v*` / `release/*`); not needed while the repo is private with one writer, PR #3.
- Config dirs on NFS (D6): none found on the shared mount, and none on the five nodes inspected (on-prem, 2026-11-04); the NFS server node was not inspected by command. The one-exporter-per-dir rule stays the guard.
- Discovered alias = home name for `~/.claude` (spec says directory-name slug, which collides); `.claude-X` → `claude-x`.
- Session liveness = `/proc/<pid>` exists (pid reuse not detected). Status words seen on real installs: `busy`, `idle`, `shell`; `shell` (meaning unknown) counts as `other`.
- Not tested anywhere yet: the arm64 binary, systemd runtime (the unit passes `systemd-analyze verify`), VictoriaMetrics ingestion.
- Example unit runs as root (agent homes are root's on the reference fleet); spec prefers a dedicated user.
- Quota absent between `stale_after` (45 m) and the end of a 60 m 429 backoff (per contract staleness rule).
