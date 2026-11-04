# Fixtures

Sanitized sample inputs so a builder can develop and test offline. **Every value in these files is synthetic or
structural.** Nothing here is real telemetry, and none of it may be used as evidence of a real account's usage.

## Provenance

Collected read-only on 2026-10-28 from a development node by `claude-sonnet-5-5`, then rewritten by script:

| File | Origin | What was kept | What was replaced or removed |
|---|---|---|---|
| `claude/transcript-sample.jsonl` | shape of a Claude Code transcript under `<config>/projects/**/*.jsonl` | field names and nesting, line types, the `usage` object shape, a duplicate-message-id case, a `<synthetic>` model case | all message content (emptied), ids (zero-padded fakes), timestamps (2026-01-01), numbers (invented), paths (`/root`), versions (`0.0.0`), model names (`model-x`) |
| `claude/sessions/4242.json` | shape of `<config>/sessions/<pid>.json` | key names and types | every value |
| `claude/oauth-usage-response.json` | field names of the Claude quota endpoint response, as used by an existing read-only collector | field names and nesting | all numbers and times invented; the extra window name is a placeholder (real names vary) |
| `claude/dot-claude.json` | invented (2026-11-03, claude-opus-5-5), shape of `<config>/.claude.json` / `~/.claude.json` | the `oauthAccount` key names the exporter reads | everything; ids are zero-padded fakes, email is `example.com`, `displayName` is a content canary |
| `claude/dot-credentials.json` | invented (2026-11-03, claude-opus-5-5), shape of `<config>/.credentials.json` | key names (`claudeAiOauth.accessToken`, `expiresAt` in Unix ms) | all values: token and refresh values are canaries (`CANARY-…`), not token-shaped; `expiresAt` 1767232800000 = 2026-01-01T02:00:00Z |
| `opencode/opencode.schema.sql` | `.schema` of a real `opencode.db`, taken from a read-only `.backup` copy (the copy was deleted) | DDL only (tables, columns, indexes) | there is no data in this file |
| `opencode/rows.sql` | invented | rows matching the schema (1 project, 1 session, 3 messages incl. an error row) | n/a, fully synthetic; message JSON uses the real key names |
| `codex/layout.txt` | names-only listing of a real Codex home | file and directory names | nothing was opened; `auth.json` is listed as expected-but-absent |
| `codex/rate-limits-response.json` | shape documented for `codex app-server` `account/rateLimits/read` | field names | numbers invented; **unverified** (not run on a logged-in client) |
| `agy/layout.md` | none on this node | n/a | described, not listed |
| `agy/usage-command.json` | shape of `agy -p /usage --output-format json` (agy CLI 1.5.0), read on-prem 2026-11-04 | key names and nesting, bucket ids `gemini-*`/`3p-*`, windows `5h`/`weekly`, the zero-cost fields (`num_turns`, `usage.*`, `command.name`) | every value invented; extended 2026-11-04 (claude-opus-5-5) with synthetic buckets: an unsafe window (`gemini-perminute`), two unknown ids that merge (`newtier-*`, `othertier-*`), and `CANARY-AGY-*` text in content fields |

## Rules for fixtures

- No token, key, cookie, email (other than `example.com`), prompt, completion, message text, or home path beyond `/root`.
- Ids are zero-padded fakes (`00000000-0000-4000-8000-...`, `msg_000...`, `ses_000...`).
- A fixture that needs real structure but would carry content keeps the key and empties the value.
- Add a new fixture only with a provenance row above.

## Checks (both must print nothing)

The bracketed letters keep this file from matching its own patterns; they are the same patterns as the vendor key prefix,
the JWT prefix, an auth-header scheme and a refresh-token value.

```
grep -rEn '@[a-z0-9-]+\.(com|vn)' fixtures | grep -v example.com
grep -rEn 's[k]-|e[y]J|Bea[r]er|r[e]fresh_token"\s*:\s*"[A-Za-z0-9]' fixtures
```

## Using the opencode fixtures

```
sqlite3 :memory: ".read fixtures/opencode/opencode.schema.sql" ".read fixtures/opencode/rows.sql" \
  "select json_extract(data,'$.providerID'), json_extract(data,'$.tokens.total') from message where json_extract(data,'$.role')='assistant';"
```
