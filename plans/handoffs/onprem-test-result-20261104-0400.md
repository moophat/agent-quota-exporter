---
handoff-version: 1
generated: 2026-11-04T04:05:00+07:00
generator: on-prem lane (claude-sonnet-5-5)
focus: "result of the on-prem integration test of v0.1.0-rc.2; one fix; what is left before v0.1.0"
workspace: moophat/agent-quota-exporter
branch: fix/once-with-quota-requires-once
---

# HANDOFF: on-prem test result, v0.1.0-rc.2

No fleet names here by rule; the full report with real names is kept outside this repo.

## Verdict

The released linux amd64 `v0.1.0-rc.2` binary works on real Claude Code profiles. Over a 30 minute run beside live sessions, token and request counters were **equal to an independent recount of the transcripts** and the exporter wrote nothing outside its own state dir. Quota values matched the fleet's own collector. One CLI defect found and fixed in this PR; one open question on the session status vocabulary.

## What was checked

| Check | Result |
|---|---|
| `sha256sum -c SHA256SUMS` on both tarballs; `--version` | OK; `v0.1.0-rc.2 (contract 0)` |
| Cloud driver `itest/run.py` on the node, released binary | ALL CHECKS PASSED (1m40s) |
| 30 min serve, discovery `add`, two real profiles, live sessions working | 32 scrapes, 0 counter decreases, 0 WARN/ERROR log lines, no token/email-like string in `/metrics` or the log |
| Token and request counters vs recount of the transcripts since first collect (dedupe by message id, max per field) | exact: 2 models, input/output/cache_read/cache_write and request counts all equal (e.g. 57 requests) |
| No replay of history at first start | confirmed: first scrape had no `agent_tokens_total` series; later values equal new lines only |
| strace of every write-type call (`openat` with write flags, rename, unlink, mkdir, chmod, utimensat, ...) | every one names the state dir; relative opens that are not `O_RDONLY`: 0; a second run with quota on: same |
| Credentials file hash and mtime before/after | identical (the live harness's own `.claude.json` did change; the exporter never opens it for writing) |
| `agent_sessions` vs `sessions/*.json` | counts match; but see status vocabulary below |
| Quota on one profile, floors unchanged (15 min, 60 min) | 2 requests in 30 min: `5h` 0.92 vs the collector's 0.90 two minutes earlier, `7d` 0.72 vs 0.72; reset timestamps equal to the second. The earlier pair (0.80 / 0.71 vs 0.77 / 0.71, 4 min apart) fits the burn rate |
| `plan` label values seen | `max`, `team` (inside the bound) |
| Credential expired on one profile | `agent_account_logged_in 0`, source stays up: correct |
| `mesh/label_map.py check` on the scrape (host set to the node handle) | default profile maps (21 series); the second profile is unmapped until declared, as designed |
| `go vet` and `go test -race` on main plus this fix | pass |

## Defect fixed in this PR

`--once-with-quota` without `--once` was accepted: the one-shot path was skipped, the server started, and the quota plugin made a real vendor request. A probe meant as a single request became a long-lived exporter. It now exits 2 with `--once-with-quota requires --once`; test added in `cmd/agent-quota-exporter/main_test.go`. (The `--help` text already says "with --once".)

## Open items for the cloud session

1. **Session status vocabulary.** Real `sessions/*.json` status words seen: `busy`, `idle`, `shell`. `shell` was stable for the whole 30 minutes on one interactive session and lands in `state="other"`. Unknown whether it means "running a shell command" (would be `active`) or "waiting in shell mode". Left unmapped; decide with the harness's own documentation, then update `sessionState` and the comment that says only `idle` is verified.
2. **Port in use is a hard failure** (`listen ...: address in use`, exit 1): fine, but the example/role should say the default 9877 differs from the fleet's 9478.
3. **D6 facts** (no change to the exporter): on the five nodes inspected, every Claude config dir that holds credentials, `projects/` or `sessions/` is on a node-local disk; none is on the shared NFS mount, and a depth-5 search of that mount found no profile dir. So today no config dir is visible to two exporters. The proposed D6 rule is therefore not yet needed; keep it as a guard if a profile is ever placed on a shared mount. One node was not inspected by command (production NFS server); its profiles could only be seen if placed under the export, and none were.
4. Stale remote branches from the earlier handoff are still present (`release/v0.1.0-rc.1`, `release/v0.1.0-rc.2` and merged `feat/*`, `docs/*`); deleting them needs Tu.
5. Not tested: arm64 binary, systemd unit, VictoriaMetrics ingestion, `account_label: email`.

## Not done, needs Tu

- Tag or publish `v0.1.0`.
- Deploy to further nodes; fleet wiring PR (role, port 9478, relabel rules); registering a Semaphore template.
- Merge of this PR (Qodo must review the latest commit first).
