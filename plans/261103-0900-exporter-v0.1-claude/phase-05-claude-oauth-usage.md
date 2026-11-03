---
phase: 5
title: Claude OAuth usage (quota)
status: done
depends: [2, 3]
---

# Phase 05 — claude-oauth-usage

## Files owned
`internal/claude/oauthusage.go` + `oauthusage_test.go`.

## Steps
1. Source `claude-oauth-usage`, created only for profiles with `quota.claude_oauth_usage: true`. Min interval
   15 m (floor), 429 backoff 60 m (floor), overridable upward via `sources:`.
2. Plugin state: `{consecutive401 int, credFingerprint string}` where fingerprint = mtime-ns + expiresAt.
3. Collect: read `.credentials.json` → locals `token`, `expiresAt`. Missing/empty token → `auth_expired` (no
   request). expiresAt ≤ now → `auth_expired` (no request). consecutive401 ≥ 2 and fingerprint unchanged →
   `auth_expired` (no request); fingerprint changed → reset counter.
4. Request: `http.Client{Timeout: 10s, CheckRedirect: return http.ErrUseLastResponse, Transport: proxy from
   env}`; `GET {base}/api/oauth/usage`, base fixed to `https://api.anthropic.com` (unexported test override).
   Headers: Authorization, `anthropic-beta: oauth-2025-04-20`, `User-Agent: agent-quota-exporter/<version>`,
   Accept json. Body limited to 1 MiB. Status: 200 → parse; 401/403 → counter++ `auth_expired`; 429 →
   `rate_limited` Backoff 60 m; 5xx/other → `upstream`; ctx timeout → `timeout`; JSON error → `parse`.
   Errors never include body, header or token; `token` variable cleared after use.
5. Parse (design §4.4) to quota samples; `agent_quota_observed_timestamp_seconds` = now.
6. Per-account dedupe: at construction, profiles with the same account uuid → only the first alias (sorted) gets
   the source; the rest log one warning. (uuid read once at startup from `.claude.json`.)

## Success criteria (stub httptest server)
- Fixture response → ratios 0.42/0.185/0.07, windows/scopes per mapping, reset timestamps, limit_info for 2
  active limits.
- Scheduler-driven at fake 1 s ticks over 1 h: exactly 4 requests; after a 429 none for 60 m; two 401s then no
  request until the credential file is rewritten.
- Stub asserts path `/api/oauth/usage`, method GET, beta header present; no other path ever requested; no
  request when token expired.
- Canary token absent from samples, errors, logs.
