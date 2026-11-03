---
handoff-version: 1
generated: 2026-11-03T19:38:00Z
generator: ak-handoff (cloud lane, claude-opus-5-5)
focus: "on-prem integration test of v0.1.0-rc.1 against real Claude profiles, PR #5 follow-through, fleet wiring, NFS ruling D6"
workspace: moophat/agent-quota-exporter
branch: feat/plan-label-contract-rows
head: 8ce6757
---

# HANDOFF: agent-quota-exporter, on-prem test of v0.1.0-rc.1

## Mission and current status

Task focus, verbatim: "on-prem integration test of v0.1.0-rc.1 against real Claude profiles, PR #5 follow-through,
fleet wiring, NFS ruling D6".

The outcome is that the exporter runs on real fleet nodes against real Claude Code profiles, with numbers that are
right and no change to any agent file. Then `v0.1.0` ships, and the fleet scrapes it with graph ids attached.

Done (cloud lane, 2026-11-03):
- Exporter v0.1 for Claude, quota included. PRs #1–#4 are merged; `main` is at 026c8a7.
- Release `v0.1.0-rc.1`: a prerelease with its tag at c5a6750. It holds the linux amd64 and arm64 tarballs plus
  SHA256SUMS.
  - The checksums verify.
  - The released amd64 binary passes the cloud end-to-end run on mocked homes:
    `plans/261103-0900-exporter-v0.1-claude/itest/`.
- Label map: agent-fleet-kit `mesh/label_map.py` and `mesh/label_map.yaml` (PR #119, merged at 706cca4).

Remaining:
- **PR #5, waiting on Qodo.** It makes the `plan` label come from `subscriptionType`, bounded to pro, max and team. It
  also adds contract rows for the two restart metrics and records rulings D5 and D6.
- **The on-prem integration test.** This is the main job here; see Exact next actions.
- **Fleet wiring.** Each node needs its harness elements declared, the listen port, a deploy role, and the relabel
  rules added to the `fleet` scrape job.
- **NFS ruling D6.** Find out which config dirs are on NFS and whether more than one exporter can see them.
- **Then `v0.1.0`.**

Urgency: none stated. Tu wants the on-prem agent to do the real integration test.

## Scope and guardrails

Workspace: `moophat/agent-quota-exporter` (Go 1.24; the only dependency is gopkg.in/yaml.v3). Fleet side:
`moophat/agent-fleet-kit` (`mesh/`, ansible roles).

In scope:
- installing and running the release binary on fleet nodes;
- test reports;
- fixes on `feat/*` or `fix/*` branches, through PRs;
- the fleet wiring PR in agent-fleet-kit.

Out of scope:
- the cockpit web (W7b). It waits for Tu's OK on layout and palette (OD #17).
- opencode or Codex plugins (later phases).
- anything that computes or shows money.

Constraints (Tu):
- **Model id.** Every commit, PR body and PR comment carries the model id (`AI-Model:` trailer or line).
- **Reviews.** Post `/agentic_review` on every PR. Merge only after Qodo has reviewed the latest commit and every
  finding is fixed or answered on its thread.
  - In agent-fleet-kit, merging to `main` plus the pull in `/mnt/shared/agentoolkit` *is* the deploy. Announce it.
- **A question is not an authorization.** Act only on an explicit instruction.
- **CI minutes are scarce.** Run tests locally on the DC. GitHub CI only if short and bounded.

Safety boundaries:
- **Read-only toward agent state.** No write, refresh or mint of any token, and no lock on agent files. The exporter
  is built that way; the test must prove it on real homes.
- **Secrets.** Never print a credential, token or email. Fleet secrets come only from bws, and never in a loop. Never
  invoke `claude` from a script.
- **Node placement.** Propose before placing load on the NFS-server production node (agent-fleet-kit `CLAUDE.md`,
  Fleet rules). Prefer the node the label map was proven on (agent-fleet-kit `mesh/`, PR #119).
- **Processes.** Track every process you start (command, PID, port, node) and stop it when done. Cron jobs need
  flock, cgroup caps and timeouts.
- **Quota endpoint.** It is private and rate-locked by IP. Enable `quota.claude_oauth_usage` on at most one profile
  per host at first. Never lower its floors: 15 min interval, 60 min backoff after a 429.
- **Exporter repo content.** No fleet-specific data: no real hosts, IPs, account names or emails. Test reports with
  real names go to the vault or agent-fleet-kit, not here.
- **Git.** No force-push, `reset --hard`, rebase or stash without Tu's word. Add files by explicit path only.

## Current state

- Branch: `feat/plan-label-contract-rows`. PR #5 is open; it was pushed at 8ce6757 before this handoff was added.
- `main`: 026c8a7 (`docs(ledger): record the v0.1.0-rc.1 release (#4)`).
- Working tree: clean apart from this handoff file.
- Stale remote branch `release/v0.1.0-rc.1`, at c5a6750: harmless. Any push to it starts a run that is refused,
  because the release already exists. The cloud proxy cannot delete it; Tu can, or the local session can with
  `git push origin --delete release/v0.1.0-rc.1`.
- Intentional local modifications: no.

## Decisions and rationale

| Decision | Rationale | Alternative rejected | Reference |
|---|---|---|---|
| Release by pushing a `release/<tag>` branch, in addition to tag push and dispatch | The cloud proxy drops tag pushes and the app token gets a 403 on dispatch | Waiting for Tu to tag | PR #3 |
| No ruleset on `v*` or `release/*` for now | Private repo with one owner; whoever can push could already publish. Revisit if the repo goes public or someone else gets write access | Repository ruleset or protected environment now | Tu, 2026-11-03; PR #3 thread |
| `plan` label bounded to pro, max, team, else `unknown` | Keeps the label bounded however upstream names plans | Passing `subscriptionType` through raw | D5, PR #5 |
| NFS config dirs: one exporter per shared dir (proposed) | Several nodes discovering the same dir multiply the counts | Dedupe across hosts (needs shared state) | D6, open |
| Listen port 9478 in the fleet role | The graph expects 9478; the exporter's default is 9877 | Changing the default | agent-fleet-kit graph |
| Relabel rules key on `node` | The `fleet` job copies the target `host` to `node` and drops it | Keying on the exporter's own `host` label | `mesh/label_map.py` docstring |

## Work performed

- Phases 01–06 implemented: config, core, account and sessions, transcripts, quota, tests.
- CI and release workflows written; the release was cut through a `release/` branch push.
- Cloud integration driver: `plans/261103-0900-exporter-v0.1-claude/itest/run.py`.
  - What it simulates: two mocked profiles, live `sleep` processes standing in for sessions, history present before
    start, streamed duplicates, a restart mid-stream.
  - What it checks: no replay of old history, 30 series equal to the ground truth, monotonic counters, no canary
    leaks, sessions 1 active and 1 idle, and profile trees byte-for-byte as the test wrote them after every phase.
- PR #5 code:
  - `internal/claude/profile.go`: `readCredentialFacts` returns the bounded plan.
  - `internal/claude/account.go`: uses that plan.
  - `TestAccountPlanIsBounded`.
  - spec text in `specs/sources.md`, `specs/metric-contract.md` and `specs/decisions.md`; `MANIFEST.md`.

0 redactions applied.

## Verification

| Check | Command | Outcome | When |
|---|---|---|---|
| Unit and e2e tests | `go vet ./... && go test -race -count=1 ./...` | pass (PR #5 head) | 2026-11-03 19:37 UTC |
| Bound on the plan label | mutation that removes `plans[...]` | `TestAccountPlanIsBounded` fails, as it must | 2026-11-03 |
| Release assets | `sha256sum -c SHA256SUMS` on downloaded assets | both OK | 2026-11-03 19:29 UTC |
| Released binary | `itest/run.py <released binary> "$(mktemp -d)"` | ALL CHECKS PASSED | 2026-11-03 19:31 UTC |
| Label map on real graph | `mesh/label_map.py check` on the itest scrape | root profile: 36 series → `harness/claude-code@<handle>`; second profile unmapped until declared | 2026-11-03 |

Not run:
- Real Claude Code profiles: there are none in the cloud. This is the on-prem test.
- Status words in real `sessions/*.json` beyond `busy` and `idle`: only those were mocked.
- The live quota endpoint: only stub-server unit tests exist, and the URL is not configurable by design.
- systemd unit, VictoriaMetrics ingestion, NFS-mounted homes.

Known pre-existing failures, not caused by this work: `mesh/tests` `test_drift`, `test_drift_review` and
`test_path_probe` fail the same way on clean agent-fleet-kit `main`.

## Open risks and blockers

- **D6, NFS double counting.**
  - Type: question. Owner: Tu or the local session (facts first).
  - Impact: if one config dir is visible to several exporters, fleet totals multiply.
  - Find out which dirs are on NFS (`findmnt -T <dir>`) and whether discovery on more than one node reaches them.
- **PR #5 merge.**
  - Type: dependency. Owner: whoever holds the session when Qodo answers.
  - Impact: until it merges, `plan` reads `unknown` in rc.1.
- **Session status vocabulary.**
  - Type: risk. Owner: local session.
  - Impact: real status words outside the mocked set land in `state="other"`; check them on real sessions.
- **Quota endpoint behaviour.**
  - Type: risk. Owner: local session.
  - Impact: the endpoint is undocumented, and a burst of bad requests rate-locks every profile on the host.
  - Enable it on one profile only and watch `agent_source_errors_total`.
- **`subscriptionType` real values.**
  - Type: question. Owner: local session.
  - Impact: if a real value (for example `enterprise`) is not in the bound, it reads `unknown`.
  - Check what it holds without printing the file (`jq -r '.claudeAiOauth.subscriptionType'`), and report it to Tu
    if it falls outside the bound.

## Exact next actions

1. **First safe step**: on the chosen node (not the NFS-server production node), read-only setup, no agent files
   touched.
   - Download the `v0.1.0-rc.1` assets into a fresh directory and run `sha256sum -c SHA256SUMS`.
   - Run `./agent-quota-exporter --version`; expect `v0.1.0-rc.1 (contract 0)`.
2. Rerun the cloud driver locally to prove the binary on this host:
   `python3 -I plans/261103-0900-exporter-v0.1-claude/itest/run.py <binary> "$(mktemp -d)"`.
   Expect `ALL CHECKS PASSED`; the run takes about 2 minutes.
3. Real-profile dry run, without quota.
   - Write a config: `host: <node handle>`, `listen: "127.0.0.1:9478"`, a `state_dir` under a fresh temp dir, discovery on.
   - Before starting, snapshot `find <each config dir> -printf '%p %s %T@\n'` excluding `projects/**/*.jsonl`, which
     Claude itself appends to.
   - Run `--once`, then serve for at least 30 min while a real session works.
   - Check:
     - nothing is replayed at first start (`agent_tokens_total` starts near 0);
     - counters rise during work and never fall;
     - `agent_sessions` matches the live sessions;
     - the non-transcript files are unchanged afterwards;
     - no token or email appears in `/metrics` or the log.
4. Quota on one profile: set `quota: { claude_oauth_usage: true }` for that profile only.
   - Expect `agent_quota_utilization_ratio{window="5h"|"7d"}` after the first collect, then at most one request per
     15 min.
   - Compare the values against what Claude Code's own `/usage` shows for that account.
5. Map the scrape: `curl -s 127.0.0.1:9478/metrics | python3 mesh/label_map.py check -`, run in agent-fleet-kit.
   Any profile other than `root` needs a Harness element on its node and a row in `mesh/label_map.yaml`.
6. NFS facts for D6: for each discovered dir, run `findmnt -T <dir>` and list which nodes can see it. Report to Tu
   with the proposed rule from `specs/decisions.md` D6.
7. Fleet wiring PR in agent-fleet-kit:
   - a role installing the binary, the systemd unit from the tarball, and `listen: :9478`;
   - the relabel output (`mesh/label_map.py relabel`) added to the `fleet` job's `metric_relabel_configs`.
   - Propose before touching the NFS-server production node.
8. Once the test passes and PR #5 is merged: cut `v0.1.0`.
   - Push the tag `v0.1.0` at that `main` commit (from on-prem), or push a `release/v0.1.0` branch.
   - Then verify the assets the same way as in step 1.
9. Write the result into this repo's `.aiworkflow/WORK_TRACKER.md` (own Active stub only) and `MANIFEST.md`, without
   real host or account names. Put the full report in agent-fleet-kit or the vault.

## Source pointers

- Release: https://github.com/moophat/agent-quota-exporter/releases/tag/v0.1.0-rc.1
- Open PR: https://github.com/moophat/agent-quota-exporter/pull/5
- Merged PRs: https://github.com/moophat/agent-quota-exporter/pull/3 (release-branch trigger and its security
  thread), https://github.com/moophat/agent-fleet-kit/pull/119 (label map)
- Spec: `specs/metric-contract.md`, `specs/sources.md`, `specs/decisions.md` (D5, D6), `specs/plan.md`
- Plan and red-team record: `plans/261103-0900-exporter-v0.1-claude/plan.md`, `design.md`, phase files
- Cloud integration run: `plans/261103-0900-exporter-v0.1-claude/itest/run.py`, `itest/README.md`
- Example config and unit: `examples/config.yaml`, `examples/agent-quota-exporter.service`
- Ledgers: `.aiworkflow/WORK_TRACKER.md`, `.aiworkflow/MANIFEST.md`
- Fleet side: agent-fleet-kit `mesh/label_map.py`, `mesh/label_map.yaml`, `mesh/tests/test_label_map.py`,
  `ansible/roles/control_box/tasks/scrape.yml`
