# Phase 4: response parser, window mapping, new metric families

## Context
`plan.md` "Metrics emitted" and the window table. Pattern: `parseUsage`/`serveQuotas` in `internal/codex/quota.go` and `usage.go`.
Independent of phases 2-3.

## Files (owned)
New: `internal/zai/parse.go`, `serve.go`, `parse_test.go`, `serve_test.go`, `fixtures/zai/quota-limit-global.json` (verified shape, invented
numbers), `quota-limit-used.json`, `quota-limit-unknown.json`, `quota-limit-authfail.json`.
Modify: `internal/core/render.go` (add the two families `agent_quota_used_calls`, `agent_quota_limit_calls`, HELP text), its contract test
list if one enumerates `FamilyNames`.

## Fixtures
`quota-limit-global.json`: the brief's verified shape: `{code, msg, success:true, data:{level:"lite", limits:[TIME_LIMIT unit5 number1
usage100 currentValue0 remaining100 percentage0 nextResetTime<ms>, usageDetails[...]; TOKENS_LIMIT unit3 number5 percentage0 (no reset);
TOKENS_LIMIT unit6 number1 percentage0 nextResetTime<ms>]}}`. `code` value is invented (verified fields are only the four names).
`used`: percentages 37 and 62.5 (float), currentValue 42, a numeric string percentage. `unknown`: a new `type`, unit 9, number 0,
`number` 12 with unit 3 (`12h`), missing `limits[]`. `authfail`: `{code:1001,msg:"...",success:false}`.

## Tests first
1. Global fixture -> samples exactly: ratio 0 for `5h/all`, `7d/all`, `monthly/tool_calls`; reset for `7d` and `monthly` only (seconds, from ms);
   `used_calls` 0, `limit_calls` 100 on `monthly/tool_calls`; `agent_account_info` plan `lite`.
2. Used fixture -> ratio 0.37, 0.625; TIME_LIMIT without `percentage` -> `currentValue/usage`; `usage` 0 -> ratio absent (no NaN/Inf); string numbers accepted.
3. Unknown fixture -> unknown type/unit skipped and counted: result reports `parse: n` in `Result.Errors`; `12h` exported; no `limits` -> `parse` error and no samples.
4. `level` unknown/missing/odd -> plan `unknown`; `team` passes; case-insensitive.
5. Duplicate mapping merge (two `unit 3 number 5`) -> max ratio, earliest reset.
6. `nextResetTime` implausible (<=0, year > 2100, NaN) -> no reset sample. Percentages negative or NaN -> absent.
7. Label values pass `^[a-z0-9_-]{1,16}$` window check; no sample name outside `core.FamilyNames()`.
8. `success:false` -> typed result the source classifies (auth codes 1000-1004 -> auth_expired, 1302/1305 -> rate_limited, else upstream).
9. `serve()` drops readings older than `core.FreshnessLimit` (inject clock) and returns nothing for an empty state.

## Steps
1. `parse.go`: tolerant envelope (`json.Number`/float, `any` for numerics), `reading{Level, Windows []window}` JSON-serialisable for state.
2. `serve.go`: windows -> `core.Sample`s with the identity labels (`host harness profile account provider`).
3. `render.go`: register the two families.

## Validation
`go test ./internal/zai/... ./internal/core/...`; the core contract test (every emitted name is a family) passes.

## Risk and rollback
Unit inference could be wrong: the closed table drops anything else visibly (`parse` counter). Rollback: revert; families are additive.
