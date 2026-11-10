# Fixtures

Sanitized sample inputs so a builder can develop and test offline. **Every value in these files is synthetic or
structural.** Nothing here is real telemetry, and none of it may be used as evidence of a real account's usage.

## Provenance

Collected read-only on 2026-10-28 from a development node by `claude-sonnet-5-5`, then rewritten by script:

| File | Origin | What was kept | What was replaced or removed |
|---|---|---|---|
| `claude/transcript-sample.jsonl` | shape of a Claude Code transcript under `<config>/projects/**/*.jsonl` | field names and nesting, line types, the `usage` object shape, a duplicate-message-id case, a `<synthetic>` model case | all message content (emptied), ids (zero-padded fakes), timestamps (2026-01-01), numbers (invented), paths (`/root`), versions (`0.0.0`), model names (`model-x`) |
| `claude/sessions/4242.json` | shape of `<config>/sessions/<pid>.json` | key names and types | every value |
| `claude/oauth-usage-response.json` | field names of the Claude quota endpoint response, as used by an existing read-only collector | field names and nesting | all numbers and times invented; the extra window name is a placeholder (real names vary). Added 2026-11-04 (opus): the key names `iguana_necktie` and `extra_usage` (top-level objects without a 5h/7d prefix) and a `limits[]` entry of kind `weekly_scoped`, as reported from on-prem responses in issue #16; only `utilization`, `percent` and `kind` are known for them, so no other field is added, and `is_active: false` on `weekly_scoped` is a guess consistent with no `agent_quota_limit_info` being seen for it |
| `claude/dot-claude.json` | invented (2026-11-03, claude-opus-5-5), shape of `<config>/.claude.json` / `~/.claude.json` | the `oauthAccount` key names the exporter reads | everything; ids are zero-padded fakes, email is `example.com`, `displayName` is a content canary |
| `claude/dot-credentials.json` | invented (2026-11-03, claude-opus-5-5), shape of `<config>/.credentials.json` | key names (`claudeAiOauth.accessToken`, `expiresAt` in Unix ms) | all values: token and refresh values are canaries (`CANARY-…`), not token-shaped; `expiresAt` 1767232800000 = 2026-01-01T02:00:00Z |
| `opencode/opencode.schema.sql` | `.schema` of a real `opencode.db`, taken from a read-only `.backup` copy (the copy was deleted) | DDL only (tables, columns, indexes) | there is no data in this file |
| `opencode/rows.sql` | invented | rows matching the schema (1 project, 1 session, 3 messages incl. an error row) | n/a, fully synthetic; message JSON uses the real key names |
| `codex/layout.txt` | names-only listing of a real Codex home | file and directory names | nothing was opened; `auth.json` is listed as expected-but-absent |
| `codex/rate-limits-response.json` | shape documented for `codex app-server` `account/rateLimits/read` | field names | numbers invented; **unverified** (not run on a logged-in client) |
| `codex/rollouts/rollout-token-count.jsonl` | one `token_count` line of a rollout, on-prem reading 2026-11-04 (Codex CLI 0.163.0, `plans/261104-1800-more-sources/plan.md`) | line and field names, nesting (`info`, `rate_limits.{primary,secondary,credits,limit_id}`) | all numbers and times invented |
| `codex/home/sessions/2026/11/0{3,4}/rollout-*.jsonl` | invented (2026-11-04, claude-opus-5-5), a two-day Codex home for the `codex-rollouts` golden test, using the shape above plus `turn_context.payload.model` and the `TokenUsage`/`RateLimitSnapshot` fields of the Codex source | line types `session_meta`, `turn_context`, `response_item`, `event_msg` (`token_count`, `user_message`, `agent_message`); a model change, repeated `token_count` events, null `rate_limits`, non-zero cache write and reasoning, a numeric `balance` | everything; ids are zero-padded fakes, paths `/root`, every text field (`cwd`, instructions, messages, tool output, `limit_name`) is a content canary (`CANARY-CONTENT…`) |
| `router/router.schema.sql` | `.schema` of a real 9router `data.sqlite`, read on-prem 2026-11-04 | DDL of the two tables a plugin may touch (`providerConnections`, `usageHistory`) | there is no data in this file |
| `router/rows.sql` | invented (2026-11-04, claude-opus-5-5) | rows matching the schema: 8 connections (healthy, rate-limited ISO and epoch ms, failed test, inactive, duplicate name, non-JSON `data`, unknown `testStatus` word) and 10 usage rows (mixed case, NULL and HTTP status, no or deleted connection, zero and non-zero cost) | n/a, fully synthetic; `status`/`testStatus` words and the `rateLimitedUntil` format follow 9router's source; every secret field holds a `CANARY-ROUTER-*` value |
| `agy/layout.md` | none on this node | n/a | described, not listed |
| `agy/usage-command.json` | shape of `agy -p /usage --output-format json` (agy CLI 1.5.0), read on-prem 2026-11-04 | key names and nesting, bucket ids `gemini-*`/`3p-*`, windows `5h`/`weekly`, the zero-cost fields (`num_turns`, `usage.*`, `command.name`) | every value invented; extended 2026-11-04 (claude-opus-5-5) with synthetic buckets: an unsafe window (`gemini-perminute`), two unknown ids that merge (`newtier-*`, `othertier-*`), and `CANARY-AGY-*` text in content fields |
| `zai/openclaw.json5` | invented (2026-11-10, claude-sonnet-5-5), structure of the OpenClaw state file read by key names on a development node: `models.providers.<id>.{baseUrl,apiKey}`, `env.vars.ZAI_API_KEY`; the real file is JSON5 with `//` comment lines | key names and nesting, comments, trailing commas | every value; keys are `CANARYZAI…` strings that pass the exporter's key shape check but match no vendor prefix |
| `zai/chelper-config.yaml` | invented (2026-11-10), shape of `~/.chelper/config.yaml` of `@z_ai/coding-helper` 0.1.1 (package source) | `plan`, `api_key` names | all values (canary key) |

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

The `internal/opencode` tests build a database in a temp dir from both files. They add their own synthetic rows inline:
in-flight rows, error rows, extra models, and canary values in content and account tables. Those rows are not
fixture files.
