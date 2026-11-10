# Phase 1: config, validation, resolve

Go: `export PATH=/usr/local/go/bin:$PATH`. Run in `/root/aqe-zai`.

## Context
`plan.md` "Profile model". Read `internal/config/{config,validate,discover}.go` and their tests first.

## Files (owned)
`internal/config/config.go`, `validate.go`, `discover.go`, `config_test.go` (or new `zai_test.go`).

## Tests first (all fail before code)
In `internal/config` (table tests, pattern of `strata_test.go`):
1. Valid: each of `key_source: openclaw|opencode|zai-helper` with `dir`, `account`, `quota: {zai_usage: true}` loads; `Resolve` returns
   `Resolved{Harness:"zai", KeySource, Region, Account, Dir, Quota}` with `Origin: "config"`.
2. Errors, each naming the profile index and key: missing `key_source`; `key_source: zcode` (message says unsupported, not "unknown");
   unknown `key_source`; missing `account`; missing `dir`; `region` not `global|cn`; `quota.zai_usage` absent or false; `quota.zai_usage`
   on harness `codex`; `key_source` or `region` on harness `claude-code`; non-boolean `zai_usage`; duplicate alias within `zai`.
3. `discovery.mode: only` and `add` never create a `zai` profile; `disable: true` removes it.
4. `Floors["zai-usage"]` is 5m/10m: a `sources.zai-usage.min_interval: 1m` without `i_know_this_can_lock_my_account` is rejected (existing
   floor test pattern); `SourceInterval`/`SourceBackoff` return the config overrides.
5. A config with `api_key: abc` under a zai profile fails (unknown field), and an inline `key: sk-...` value fails the secret scan.

## Steps
1. Add `HarnessZAI = "zai"` to `Harnesses`; `quotaKeys["zai_usage"] = HarnessZAI`; `Floors["zai-usage"]`.
2. `Profile`: `KeySource string yaml:"key_source"`, `Region string yaml:"region"`. `Resolved`: same two fields; `Resolve` copies them.
3. `Validate`: the rules in tests 2. Export `ZAIKeySources = []string{"openclaw","opencode","zai-helper"}` for the plugin.
4. Do not touch discovery code except to prove test 3.

## Validation
`go test ./internal/config/... && go vet ./...` pass; `go test ./...` still passes (no other package reads the new fields).

## Risk and rollback
Low; additive. Revert the commit.
