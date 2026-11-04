# Decisions

Open and settled decisions for agent-quota-exporter. The maintainer decides; an agent records the ruling with its date
and who ruled. If this file and another spec document disagree, this file is the decision.

| # | Decision | Status |
|---|---|---|
| D1 | Licence | **LOCKED 2026-10-28**: Apache-2.0 |
| D2 | Implementation language (plan decision #13) | **LOCKED 2026-10-28**: Go |
| D3 | Public repository timing | OPEN |
| D4 | Per-session metrics | OPEN |

---

## D1. Licence: LOCKED Apache-2.0

**Ruled by Tu, 2026-10-28:** "just do what node exporter and normal popular exporters did".

node_exporter and the prometheus/ organisation's exporters are Apache-2.0, so this project is Apache-2.0. The `LICENSE`
file is the standard Apache-2.0 text. MIT was the alternative considered and is not pursued.

## D2. Language: LOCKED Go

**Ruled by Tu, 2026-10-28:** "Golang then" (after "Compiled language is better for agent").

Considered and rejected: Python edited in place on nodes (a node-local hotfix drifts from the repo; fixes go PR →
build → rollout, and vendor endpoint/field mappings live in config so most breakages need no rebuild), and PyInstaller
(loses in-place editing anyway, glibc-bound rather than static, one-file mode unpacks to /tmp on every start, carries a
whole interpreter). The original rationale follows. The exporter is a long-running, read-only daemon deployed to many heterogeneous nodes, where a
single static binary and a predictable footprint outweigh Python's faster prototyping, and most Prometheus exporters are
Go. The comparison table and the stdlib-only Python alternative are in `plan.md` (section "Go vs Python"). The contract
and fixtures are language-neutral, so this does not block phase 0 or 1 design work.

## D3. Public timing: OPEN

Private until phase 5 (first public release) is the plan's default; not ruled. Once public, `main` becomes protected
and PRs are required (see `AGENTS.md`, Git).

## D4. Per-session metrics: OPEN

Whether a per-session metric set is wanted at all. Current design: off by default, no session ids as labels on hot
series (cardinality rule in `metric-contract.md`).

## D5. Claude plan label and contract rows: LOCKED (Tu, 2026-11-03)

- `claudeAiOauth.subscriptionType` in `.credentials.json` is allow-listed for `claude-account` (`sources.md`). It
  feeds `agent_account_info{plan}`, bounded to `pro`, `max`, `team`; anything else reads `unknown`.
- `agent_quota_exporter_state_epoch` and `process_start_time_seconds` get rows in the contract's health table. Both
  were already emitted and described in prose, so this documents them and is non-breaking (contract stays 0).

## D6. Claude config dirs on NFS: OPEN, not needed today (facts recorded, design not ruled)

Tu, 2026-11-03: some Claude config dirs on the fleet are on NFS. Not yet known: which dirs, and whether one dir is
visible to more than one node's exporter. If it is, every exporter that discovers it counts the same transcripts,
so fleet totals are multiplied by the number of nodes. Proposed rule, pending a ruling: a config dir on a shared mount
is read by exactly one exporter (the node that owns the session); on the other nodes, narrow `discovery.roots`
or use `discovery.mode: only` with roots that leave it out. The config has no per-dir exclude today.

On-prem inspection, 2026-11-04 (`plans/handoffs/onprem-test-result-20261104-0400.md`):
- On the five nodes inspected, every Claude config dir that holds credentials, `projects/` or `sessions/` is on a
  node-local disk, and none is on the shared mount.
- A depth-5 search of the shared mount found no profile dir.
- Not inspected by command: the NFS server node itself. A profile there would be visible to other exporters only if
  placed under the export, and the mount search found none. Its node-local profiles were not checked.

So no shared config dir is known today, and the rule is not applied yet. It stays OPEN as the guard: apply it if a
profile is ever placed on a shared mount, and re-check when the NFS server node is inspected.

## D7. Model attribution, no Co-Authored-By: LOCKED (Tu, 2026-11-04)

- No commit and no squash message in this repo carries a `Co-Authored-By` trailer.
- The model is named in every commit message (`AI-Model:` and `Model:` trailers), every PR body (`**AI-Model:**`
  line) and every PR comment or review reply (`AI-Model:` line), for failure attribution.
- Where an agent's environment forbids the exact model id in pushed artifacts, it writes the generic family name
  (`opus`, `sonnet`, `haiku`, ...) instead. Commits pushed before this ruling keep what they have; history is not
  rewritten. Checked on `main` after the v0.2 merge (14aeb20):
`git log -1 --format=%b` shows none.

## D8. SQLite read-marks in a live writer's -shm: ACCEPTED (Tu, 2026-11-04)

With a writer present, a read-only SQLite reader attaches to the existing `-shm` and writes read-marks into it, as
every SQLite reader does; it never writes the database or `-wal`. Accepted as SQLite's own coordination, not a write
of agent state. The exporter still never creates a sidecar (an idle WAL database is read immutable) and refuses a
sidecar that is a link, a hard link or not a regular file. Running the exporter as each database's owner is not
required.

## D9. Provider balances: NOT BUILT (Tu, 2026-11-04)

No balance plugin. The vendors' prices are not known in real time, so a balance cannot be turned into anything the
fleet can act on, and it would carry money and a unit label the contract forbids. Codex credits (a vendor-reported
credit count in the rollouts) are unaffected.

## D10. Idle memory target: ~16 MB (Tu, 2026-11-04)

The v0.2 plan's "under 15 MB" is replaced by about 16 MB. Measured: 15.7 to 16.9 MB RSS plateau with 11 profiles and
a 300k-row router DB; the extra over v0.1 is the pure-Go SQLite driver's fixed cost. A cgo driver (smaller, but no
static release build) is not wanted.

## Also recorded in `plan.md`, not yet ruled

Whether the Claude quota plugin is on by default for discovered profiles or always explicit opt-in (plan: always
explicit).
