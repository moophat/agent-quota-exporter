---
phase: 3
title: Claude account + sessions
status: done
depends: [2]
---

# Phase 03 — claude-account, claude-sessions

## Files owned
`internal/claude/{profile.go,account.go,sessions.go}` + tests; fixture additions under `fixtures/claude/`
(synthetic `.claude.json`, `.credentials.json` with canary values, provenance rows in `fixtures/README.md`).

## Steps
1. `profile.go`: `Profile{Alias, Dir, AccountAlias, QuotaOptIn, Origin}`; helper `claudeJSONPath(dir)` =
   `dir/.claude.json` if exists, else if `filepath.Base(dir)==".claude"` → `filepath.Dir(dir)/.claude.json`.
2. `account.go` source `claude-account` (60 s): decode `.claude.json` into a struct holding only
   `oauthAccount.accountUuid` and (only when `account_label: email`) `oauthAccount.emailAddress`. Account label:
   config alias > (email mode: email) > `acct-`+hex(sha256(uuid))[:8] > `unknown`. Decode `.credentials.json`
   into `{claudeAiOauth:{expiresAt int64 (ms), accessToken json.RawMessage}}`; record only presence of a
   non-empty accessToken and expiresAt; zero the read buffer. Emits `agent_account_info{plan="unknown"}`,
   `agent_account_logged_in` (1 iff token present and expiresAt > now), `agent_credential_expiry_timestamp_seconds`
   (absent if no expiresAt). Shares resolved account label + uuid with other plugins via a small
   `AccountCache` in the profile (mutex-guarded), so transcripts/sessions/quota use the same `account` value.
   Missing files → logged_in 0, reason `not_found` only for `.claude.json` absent? No: absent credentials is a
   valid state (logged out) → success with logged_in 0; unreadable (EACCES) → reason `permission`.
3. `sessions.go` source `claude-sessions` (15 s): list `dir/sessions/*.json`, decode `{pid int, status string}`
   only; alive = `/proc/<pid>` exists (injectable for tests); state map design §4.3; emit `agent_sessions` for
   active/idle/other always (0 included). No `sessions/` dir → emit zeros (success).

## Success criteria
- Account tests: default `~/.claude` sibling `.claude.json` found; config alias wins; hash label stable and 8 hex;
  email used only in email mode; expired token → logged_in 0; canary accessToken never in samples/errors/logs.
- Sessions tests: fixture 4242 with alive stub true → idle 1; alive false → all 0; status `busy` → active.
