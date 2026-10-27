# Agent Instructions

*These rules apply to every task in this project unless explicitly overridden. Bias: caution over speed on non-trivial work. Use judgment on trivial tasks.*

0. **A question is not an authorization.** When Tu asks how, what, whether, or what we should or have to do, answer
   and stop. Act only on an explicit instruction to act. No agent or sub-agent authorizes an action on Tu's behalf, and
   an answer never launches the work it describes (Tu, 2026-10-24).
1. **Think before coding.** State assumptions. Inspect first (files, git, live state); ask only what those cannot answer, with your recommended default; how many questions and when is your call. If a request has several plausible readings, name them and say which you took. Push back when a simpler approach exists. If still confused after inspecting, stop and name what is unclear.
2. **Simplicity first.** Minimum code that solves the problem. Nothing speculative: no features beyond what was asked, no abstractions for single-use code.
3. **Surgical changes.** Touch only what you must. Don't improve adjacent code. Match existing style. Remove only the imports and code your own change orphaned; mention unrelated dead code, don't delete it.
4. **Goal-driven execution.** Before you start, define the check that proves it works (a test, build or command you can run). Loop until it passes and show its output.
5. **Use the model only for judgment calls.** Classification, drafting, summarisation, extraction. Routing, retries, status-code handling and deterministic transforms are code. If code can answer, code answers.
6. **Spend context deliberately.** Read only what the task needs. Bulk reading or searching goes to a sub-agent that returns a summary, not the files. When the context gets heavy, summarise into the ledger and hand off. This rule is about your own reading and chatter, never about the size or format of the code you write: source stays readable. Do not report token budgets or breaches.
7. **Surface conflicts, don't average them.** Pick one (more recent, more tested), say why, and flag the other in your report.
8. **Read before you write.** Read exports, callers and shared utilities first; if unsure why code is shaped a certain way, check its history (git log/blame) before asking.
9. **Tests verify intent, not just behaviour.** A test that cannot fail when the business logic changes is wrong.
10. **Checkpoint after every significant step** in the ledger or handoff: done, verified, left. A checkpoint is not a commit and not a message to Tu. If you lose track, rebuild the state from the ledger, files and git, write it down, and continue.
11. **Match the codebase's conventions,** even if you disagree; surface a harmful one, don't fork it silently.
12. **Fail loud.** "Completed" is wrong if anything was skipped; "tests pass" is wrong if any were skipped. Say what you did not verify. An orchestrator treats a sub-agent's "done" as a claim until it has checked.

---

## WHAT THIS PROJECT IS

**agent-quota-exporter**: a vendor-agnostic, read-only Prometheus exporter for AI coding-agent subscription quota
windows and per-agent usage, built for several accounts in several workspaces on one machine. It is an open-source
project of its own and knows nothing about any user's fleet. Status: pre-release, design phase (no implementation yet).
Read `README.md`, `docs/plan.md`, `docs/metric-contract.md`, `docs/sources.md`, `.aiworkflow/WORK_TRACKER.md` and
`.aiworkflow/MANIFEST.md` at session start.

What belongs here: the plan, the metric contract, the source catalogue, sanitized synthetic fixtures, and (later) the
exporter source, tests, packaging and example dashboards/alerts, all written generically.

What never does:
- **Secrets**: no token, key, password, cookie or credential file content, real or example-looking. Fixtures use
  invented values only.
- **Fleet-specific config**: no real host names or IPs, account names or emails, inventories, graph ids, topology,
  or label-mapping tables of any particular user. Examples use `example.com` and neutral aliases (`node-a`, `acct-a1`).
- **Real telemetry or message content** from any agent: transcripts, prompts, tool output, session ids, paths
  beyond `/root` or `/home/<alias>`.
- Collected data, build output, venvs, local DB copies.

Hard rules for this project:
- Read-only toward agent state; no code path may refresh, mint or write a token (see `README.md` principles).
- Before committing a fixture, run the checks in `fixtures/README.md`; they must return empty.
- Do not commit or push unless the maintainer (or the orchestrating agent acting on the maintainer's instruction) says so.
- Contract changes (`docs/metric-contract.md`) are breaking or non-breaking per its Versioning section; state which.
