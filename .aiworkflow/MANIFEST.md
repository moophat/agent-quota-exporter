# MANIFEST: agent-quota-exporter
**Last-Updated:** 2026-11-03T19:35:00Z

Status values: planned, in-progress, done. Updated 2026-11-03 by claude-opus-5-5. Licence Apache-2.0 (locked). Language Go (locked). v0.1 (Claude only) merged to `main` (PR #1, 2026-11-03); release v0.1.0-rc.2 published 2026-11-03 (prerelease, tag at 19852a0; rc.1 at c5a6750 predates the plan label). Release by tag push, main-only dispatch, or a `release/<tag>` branch push. Tests: all passing (`go test -race ./...`). Contract 0, validated by the golden and contract-parser tests.

| Component | Status | Notes |
|---|---|---|
| Spec: plan, metric contract, sources, decisions | done (draft v0) | design phase; `specs/` |
| Repo guards: gitleaks config, pre-commit, secrets workflow | done | copied from the TSB repos |
| Fixtures | done (synthetic) | see `fixtures/README.md` |
| Core: config loader and validation | done | `internal/config`, feat/phase1-core |
| Core: profile discovery | done (Claude) | `internal/config/discover.go` |
| Core: scheduler, cache, rate gates, state store | done | `internal/core` |
| Core: `/metrics`, `/healthz`, health metrics | done | `internal/core/render.go`, `http.go` |
| Plugin: claude-transcripts, claude-sessions, claude-account | done, awaiting on-prem test | `internal/claude` |
| Plugin: opencode-db (and opencode-http) | planned | not in v0.1 (maintainer, 2026-11-02) |
| Plugin: claude-oauth-usage (quota) | done, awaiting on-prem test | opt-in; `internal/claude/oauthusage.go` |
| Plugin: codex-state, codex-rate-limits | planned | phase 3, rate-limits opt-in and gated |
| Plugin: agy | planned | phase 3, discovery first |
| Provider plugins: Anthropic/OpenAI admin usage, balances | planned | phase 4 |
| OTLP output | planned | phase 4 |
| Packaging: binary/package, systemd unit, container | planned | phase 5 |
| Example dashboards and alert rules | planned | phase 5 |
| CI: tests, golden metrics, secret-scan of fixtures | done | `.github/workflows/ci.yml`, `release.yml` |

## Known issues / deviations (v0.1)
- Release triggers have no repository-side control yet (no ruleset on `v*` / `release/*`, no protected environment on `publish`); maintainer decision, PR #3.
- Config dirs on NFS (D6): a dir visible to several nodes' exporters would be counted once per node; which dirs, and the rule, are open.
- Discovered alias = home name for `~/.claude` (spec says directory-name slug, which collides); `.claude-X` → `claude-x`.
- Session liveness = `/proc/<pid>` exists (pid reuse not detected); busy status words beyond `idle` unverified.
- Example unit runs as root (agent homes are root's on the reference fleet); spec prefers a dedicated user.
- Quota absent between `stale_after` (45 m) and the end of a 60 m 429 backoff (per contract staleness rule).
