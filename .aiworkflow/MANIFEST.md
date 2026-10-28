# MANIFEST: agent-quota-exporter
**Last-Updated:** 2026-10-28T13:05:00+07:00

Status values: planned, in-progress, done. Updated 2026-10-28 by claude-sonnet-5-5. Licence Apache-2.0 (locked). Design documents exist; no code does.

| Component | Status | Notes |
|---|---|---|
| Spec: plan, metric contract, sources, decisions | done (draft v0) | design phase; `specs/` |
| Repo guards: gitleaks config, pre-commit, secrets workflow | done | copied from the TSB repos |
| Fixtures | done (synthetic) | see `fixtures/README.md` |
| Core: config loader and validation | planned | phase 1 |
| Core: profile discovery | planned | phase 1 |
| Core: scheduler, cache, rate gates, state store | planned | phase 1 |
| Core: `/metrics`, `/healthz`, health metrics | planned | phase 1 |
| Plugin: claude-transcripts, claude-sessions, claude-account | planned | phase 1 |
| Plugin: opencode-db (and opencode-http) | planned | phase 1 (db), later (http) |
| Plugin: claude-oauth-usage (quota) | planned | phase 2, opt-in |
| Plugin: codex-state, codex-rate-limits | planned | phase 3, rate-limits opt-in and gated |
| Plugin: agy | planned | phase 3, discovery first |
| Provider plugins: Anthropic/OpenAI admin usage, balances | planned | phase 4 |
| OTLP output | planned | phase 4 |
| Packaging: binary/package, systemd unit, container | planned | phase 5 |
| Example dashboards and alert rules | planned | phase 5 |
| CI: tests, golden metrics, secret-scan of fixtures | planned | phase 1 |
