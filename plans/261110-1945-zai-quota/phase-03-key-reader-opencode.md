# Phase 3: `opencode` key reader

## Context
`plan.md` "Key sources", row `opencode`. Read `internal/sqlro` (allow-list, immutable idle-WAL handling) and `internal/opencode/opencode.go`
for how the DB is opened. The spec's never-read list names table `credential`; this phase adds one narrow, tested exception.

## Files (owned)
New: `internal/zai/opencode.go`, `opencode_test.go`, `fixtures/zai/opencode-auth.json`, `fixtures/zai/opencode-credential.sql`
(DDL copied from the real schema `credential` table, plus invented rows with canary keys).
Modify: none outside `internal/zai`.

## Tests first
1. `auth.json` fixture with `zai-coding-plan: {type:"api", key}` -> key, region `global`; `zhipuai-coding-plan` only -> `cn`; both -> the
   `zai-coding-plan` one; `type:"oauth"` or `wellknown` entries for those ids are skipped; ids with trailing `/` are normalised as
   opencode does.
2. DB: build a temp sqlite from the SQL fixture (pattern: opencode tests): row `integration_id='zai-coding-plan'`,
   `value='{"type":"key","key":"..."}'` -> key; two rows same integration: `active=1` wins, else greatest `time_updated`; `type`
   `oauth`/`external` skipped; no matching row -> `not_found`; DB missing and no `auth.json` -> `not_found`.
3. Order: `auth.json` match wins over the DB; DB used only when `auth.json` is absent or has no match.
4. Allow-list: a test asserts the only selected expressions are `credential.integration_id`, `credential.active`, `credential.time_updated`,
   `JSON(value,$.type)`, `JSON(value,$.key)`, queried with `ByKeys` on the four ids; selecting `value` whole is impossible (sqlro rejects it).
5. Idle WAL database (no `-wal`/`-shm`) reads through the existing immutable path; a locked DB gives `timeout`; a non-JSON `value` is skipped, not fatal.
6. Errors contain neither key nor path. `fp` for DB source = `time_updated` + `integration_id` of the chosen row.

## Steps
1. Implement `readOpencode(dir)` returning the same `key` seam; reuse `sqlro.Open(path, allow)` per call, close immediately.
2. Register in `readerFor("opencode")`.

## Validation
`go test ./internal/zai/... ./internal/sqlro/...`; `go vet ./...`.

## Risk and rollback
Opencode changes credential storage often (file to DB already happened): an unknown future store gives `not_found`. Rollback: remove file and the `readerFor` entry.
