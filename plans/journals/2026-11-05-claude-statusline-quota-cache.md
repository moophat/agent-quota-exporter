---
title: Claude statusline quota cache
date: 2026-11-05
summary: "Issue #20 implemented and verified locally; no commit or release"
---

# Claude statusline quota cache

**Model:** codex

Read-only Claude statusline cache is bound to the resolved /root/.claude profile. It reuses OAuth parsing and sample construction; scheduler fallback preserves persisted OAuth gates and suppresses duplicate quota series. Cache reads are shared across polling and fallback checks at usage_interval, and timestamped readings expire at stale_after.

Configuration accepts a per-profile cache path before the exporter's CK_USAGE_CACHE_PATH and default /tmp path. README/examples document PrivateTmp and read-only bind behavior; CHANGELOG records the addition. The systemd unit and specs are unchanged.

Verification: focused Statusline tests pass; PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=auto go vet ./... exits 0 and go test -race ./... exits 0. The existing SQLite permission test passed as nobody after rerunning from its package directory (its first invocation used the wrong fixture-relative working directory). Independent bounded simplification review identified repeated fallback file reads; shared memoization fixes that with a cadence regression test.

Not verified: live-node reading age, real AgentKit cache, real vendor endpoint, or systemd runtime. No credentials read from live state, no claude invocation, no commit, push, or tag. AgentWiki publish skipped.

> Historical work record — not durable authority. Prefer docs/specs/ADRs for current decisions.
