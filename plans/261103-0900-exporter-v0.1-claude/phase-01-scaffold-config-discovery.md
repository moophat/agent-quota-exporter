---
phase: 1
title: Scaffold, config, discovery
status: done
depends: []
---

# Phase 01 — scaffold, config, discovery

## Files owned
`go.mod`, `go.sum`, `cmd/agent-quota-exporter/main.go` (stub), `internal/config/{config.go,validate.go,discover.go,duration.go}`
and their `_test.go`, `examples/config.yaml`.

## Steps
1. `go mod init github.com/moophat/agent-quota-exporter`, go 1.24; single dep `gopkg.in/yaml.v3`.
2. Types mirror `specs/plan.md` §Config format exactly (version, listen, host, state_dir, labels, discovery,
   metrics, defaults, profiles, sources, otlp). `Duration` type parses Go durations from YAML strings.
3. Load: `--config` flag or `AGENT_QUOTA_EXPORTER_CONFIG`; no file → defaults (discovery add). Decoder
   `KnownFields(true)`.
4. Validate (design §3.4): version==1; duplicate alias per harness; inline secret-looking string anywhere
   (`sk-`, `eyJ`, `Bearer `, `-----BEGIN`); static label name regex `^[a-zA-Z_][a-zA-Z0-9_]*$`, not a contract label,
   total label count ≤ 12; `sources.<name>.min_interval`/`backoff_429` below floor without
   `i_know_this_can_lock_my_account: true`; `otlp.endpoint != ""` and `metrics.per_session: true` → error "not
   supported in this version"; `metrics.account_label` ∈ {alias, email}; `discovery.mode` ∈ {off, add, only}.
   Harness not `claude-code` → warning, profile skipped (returned in a `Skipped` list for logging).
5. Discovery (design §3.5) for claude-code: `$CLAUDE_CONFIG_DIR` (exporter env) + each root glob, one level:
   entries `.claude` / `.claude-*` that are directories containing `projects/`, `sessions/` or
   `.credentials.json`. Resolve symlinks (`filepath.EvalSymlinks`) and dedupe by real path against config
   profiles (config wins). Alias rules: `.claude` → slug(home basename); `.claude-X` → slug(`claude-X`);
   `$CLAUDE_CONFIG_DIR` → slug(basename) unless basename is `.claude`; collisions `-2`, `-3` in sorted path order.
   slug = lowercase, `[^a-z0-9-]`→`-`, trimmed. `mode: only` → discovered profiles only when no config profiles?
   No: spec `only` = discovered only; `off` = config only; `add` = both.

## Success criteria (mechanical)
- Table tests: each validation error case returns an error naming the key; valid example config loads.
- Discovery test on a temp tree (`/tmp/x/root/.claude`, `/tmp/x/home/a/.claude`, `/tmp/x/home/a/.claude-work`,
  `/tmp/x/home/b/.claude.json` file, `/tmp/x/home/c/.claude` empty dir, a symlink duplicate) yields exactly
  aliases `root`, `a`, `claude-work`, nothing for b/c, no duplicate.
- `go vet ./... && go test ./internal/config/...` green.
