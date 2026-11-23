# Decisions

Settled and open design decisions for agent-quota-exporter, with their rationale. The maintainer decides. If this file
and another spec document disagree, this file is the decision.

| # | Decision | Status |
|---|---|---|
| D1 | Licence | Settled 2026-10-28: Apache-2.0 |
| D2 | Implementation language | Settled 2026-10-28: Go |
| D3 | Public repository | Settled 2026-11-23: public, after removing internal process files |
| D4 | Per-session metrics | Settled 2026-11-13, amended 2026-11-15: opt-in `metrics.per_session` |
| D5 | Claude plan label and health rows | Settled 2026-11-03 |
| D6 | Claude config dirs on shared mounts | Open: guidance only, no exclude option |
| D8 | SQLite read-marks in a live writer's `-shm` | Settled 2026-11-04: accepted |
| D9 | Provider balances | Settled 2026-11-04: not built |
| D10 | Idle memory target | Settled 2026-11-04: about 16 MB |
| D11 | Plain account labels in events | Settled 2026-11-08 |
| D12 | One freshness rule for every source | Settled 2026-11-08 |

---

## D1. Licence: Apache-2.0

Follow what node_exporter and the prometheus/ organisation's exporters do: Apache-2.0. The `LICENSE` file is the
standard Apache-2.0 text. MIT was the alternative considered.

## D2. Language: Go

The exporter is a long-running, read-only daemon deployed to many heterogeneous machines, where a single static binary
and a predictable footprint outweigh faster prototyping, and most Prometheus exporters are Go. Considered and
rejected: Python edited in place on each machine (a local hotfix drifts from the repository; fixes go through a
release instead, and vendor endpoint and field mappings live in config so most breakages need no rebuild), and
PyInstaller (glibc-bound rather than static, one-file mode unpacks to /tmp on every start, carries a whole
interpreter). The comparison table is in `plan.md` ("Go vs Python").

## D3. Public repository

The project is developed in the open. Internal planning notes and agent-process files were removed
before publication; `specs/` carries the contracts and the rationale that matter to users and contributors.

## D4. Per-session metrics: opt-in

Per-session series exist behind `metrics.per_session: true` only (default off), as fixed 0/1 state sets per live
session with a per-profile cap (`max_sessions_per_profile`, default 50), and a `session` label that is the sanitized
harness session name (usually a project folder name). Never a session id, path, prompt, task or screen text. See
`metric-contract.md`, "Session and job metrics (opt-in)".

Amended 2026-11-15: `agent_session_info` carries `pid` (only when the session file proves a live process on this
host), so a consumer can join a session row to an OS process exactly. Joining by tmux session name proved ambiguous
when several processes share one tmux session. Prompts, task text, screen text, paths and session ids still never
appear.

## D5. Claude plan label and contract rows

- `claudeAiOauth.subscriptionType` in `.credentials.json` is allow-listed for `claude-account` (`sources.md`). It
  feeds `agent_account_info{plan}`, bounded to `pro`, `max`, `team`; anything else reads `unknown`.
- `agent_quota_exporter_state_epoch` and `process_start_time_seconds` get rows in the contract's health table. Both
  were already emitted and described in prose, so this documents them and is non-breaking (contract stays 0).

## D6. Claude config dirs on shared mounts: open

If one Claude config dir is visible to more than one machine's exporter (for example on an NFS mount), every exporter
that discovers it counts the same transcripts, so totals across machines are multiplied. Guidance until a dedicated
option exists: a config dir on a shared mount is read by exactly one exporter (the machine that runs the sessions);
on the other machines, narrow `discovery.roots` or use `discovery.mode: only` with roots that leave it out. The config
has no per-dir exclude today.

## D8. SQLite read-marks in a live writer's -shm: accepted

With a writer present, a read-only SQLite reader attaches to the existing `-shm` and writes read-marks into it, as
every SQLite reader does; it never writes the database or `-wal`. Accepted as SQLite's own coordination, not a write
of agent state. The exporter still never creates a sidecar (an idle WAL database is read immutable) and refuses a
sidecar that is a link, a hard link or not a regular file. Running the exporter as each database's owner is not
required. Measured (strace, `internal/sqlro`, 2026-11-04): when the exporter cannot open the live `-shm` read-write
(another owner, mode 0644, run as `nobody`), SQLite gets `EACCES`, falls back to a read-only open and mapping, and the
read succeeds, with a live writer and after it was killed. So a non-owner exporter reads without writing there at all.

## D9. Provider balances: not built

No balance plugin. The vendors' prices are not known in real time, so a balance cannot be turned into anything an
operator can act on, and it would carry money and a unit label the contract forbids. Codex credits (a vendor-reported
credit count) are unaffected.

## D10. Idle memory target: about 16 MB

About 16 MB RSS, with a ceiling of 17 MB on the reference load (11 profiles, a 300k-row router database): above 17 MB
is a regression to investigate. Measured: 15.7 to 16.9 MB plateau; the extra over v0.1 is the pure-Go SQLite driver's
fixed cost. A cgo driver (smaller, but no static release build) is not wanted.

## D11. Plain account labels in events

Events use the same public account label as metrics: the configured alias, the existing `acct-xxxxxxxx` short stable
hash, or the email when `account_label: email` is configured. Identity and usage `account` and `metric_account` are
the same label when attribution is available. There is no separate keyed-ID policy; allow-listed identity metadata
may contain emails. Secret, credential and prompt/content bans remain in force. The parity profile permits configured
email labels.

`events.identity_key_file` is accepted only for compatibility and ignored with one startup log message when
configured; no key is read or required.

The event schema stays 1: this is a contract correction with changed account semantics, and `metric_account` is
additive compatibility data that older rows may omit. Retained old rows are not rewritten or silently relinked.
Consumers of mixed history must account for the old versus new account semantics. The metric contract stays 0.

## D12. One freshness rule for every source

A reading is stale iff its age exceeds `max(defaults.stale_after, 2*P + D + 15s)`, where `P` is the source's
effective poll interval and `D` its collection timeout. Any positive `stale_after` is valid. The legacy
statusline-specific setting is accepted but ignored. This supersedes earlier per-source freshness exceptions; a quota
reset can end eligibility earlier. Vendor polling cadence is unchanged.

The optional [event-contract.md](event-contract.md) replaces the earlier metrics-only history boundary. Its
freshness and failure policy supersedes a blanket one-hour backoff for ordinary failures and the reading of metric
staleness as history deletion. It preserves the 900 s floor of direct Claude quota calls, real rate-limit cooldowns,
guard stops and the no-refresh rule.

Known gap: the agy client may refresh its own credentials when `agy-usage` runs it, which conflicts with the
no-refresh boundary of the event policy; `agy-usage` stays opt-in and documented as such. Codex live-response field
names come from a third-party client and are not vendor-documented.

## Also recorded in `plan.md`

Claude quota calls are always an explicit opt-in per profile; they are never on by default for discovered profiles.
