# AGENTS.md — agent-quota-exporter

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

# Agent Instructions

## FILE STRUCTURE

Read these files at session start:
- `README.md` - Repo overview and principles
- `.aiworkflow/WORK_TRACKER.md` - Active + paused work only
- `.aiworkflow/MANIFEST.md` - Project-wide status
- `.aiworkflow/SESSION_{branch-slug}.md` - Last session state for your branch (if exists)
- `specs/plan.md`, `specs/metric-contract.md`, `specs/sources.md`, `specs/decisions.md` - the spec. Read-only unless the task is to change it.

If a referenced file or directory does not exist, skip it and continue.

## WORKING MODE

This repo uses one workflow: direct work.

**Workflow**
1. **Run `python .aiworkflow/rotate-work-tracker.py`** at session start
2. Break down work into steps (write this down!)
3. Add entry to `WORK_TRACKER.md` Active section (see template below)
4. Work through steps, mark `[x]` as you complete them
5. Update `SESSION_{branch-slug}.md` every 15 minutes
6. When all complete:
   - Update `MANIFEST.md` component status if it implements a planned component
7. **Run `python .aiworkflow/rotate-work-tracker.py`** at session end

---

## MANIFEST.md RULES

MANIFEST is the **project-wide status board**. It answers: "what's the state of the whole project?"

### Allowed content:
- Active branches + PRs (one line each)
- Component matrix (one line per component: status + name + branch)
- Known issues / deferred tech debt (things that affect future work)
- Metric contract summary (version + validation status)
- Implementation artifact references (key file paths, doc links)

### BANNED from MANIFEST — put it in plan.md instead:
- Per-phase progress details ("Phase 1: DONE, Phase 2: IN PROGRESS...")
- Effort tracking / time estimates ("Phase 1: 1.5h estimated / 2h actual")
- Step-by-step implementation logs
- "Next Steps" that duplicate what's in the plan file
- Commit hashes for individual phases
- Test count updates per phase (just keep one global "Tests: X passing")

### Update frequency:
- Component status: when a component changes state (started, paused, completed)
- Known issues: when discovered or resolved
- Contract: when the metric contract changes
- Add `**Last-Updated:** {ISO timestamp}` header

---

## WORK_TRACKER.md RULES

WORK_TRACKER is a **permanent ledger** with a 3-layer queue: Active (hot), Log (warm, max 30), Archive (cold). Stubs are never deleted — only moved between layers by the rotation script.

### Active entry template (max 30 lines):
The stub block (between `BEGIN/END STUB` markers) is embedded in each Active entry. The agent maintains it as part of normal work. The script extracts it mechanically on compression — no separate stub-writing step needed.
```markdown
## Active — {Title} | {YYYY-MM-DD}
<!-- BEGIN STUB: {YYYY-MM-DD} | {Title} -->
### {Title} | {YYYY-MM-DD}
**Meta:** {branch} | {model-id} | `{plan-link}`
**Outcome:** {current status}
**Key:** {decisions made, comma-separated}
**Pending:** {open items or none}
<!-- END STUB: {YYYY-MM-DD} | {Title} -->
- [x] Decision made
- [ ] Decision pending
```

### Agent rules (CRITICAL):
1. **Run `python .aiworkflow/rotate-work-tracker.py`** FIRST at session start
2. **APPEND your new `## Active` entry** — do NOT touch, edit, or delete any existing entries
3. **Update ONLY your own stub block** as you work (Outcome, Key, Pending lines)
4. **Run the script again** at session end
5. **NEVER delete, move, or compress anything** — that is the script's job, not yours

### What the script does (idempotent, safe to run anytime):
1. **COMPRESS**: if multiple `## Active` entries exist, keeps newest, moves older stub blocks to `## Log`. The agent does NOT decide what to compress.
2. **ROTATE**: if stubs in `## Log` exceed 30, moves oldest to archive.

### BANNED from WORK_TRACKER:
- Verbose step-by-step implementation logs (→ plan.md)
- Per-file change lists (→ git log)
- Postmortems or "what went wrong" narratives (→ plans/reports/)
- CSS property lists, test output details (→ plan.md or commit messages)

### File structure:
```markdown
# WORK_TRACKER
**Last-Updated:** {ISO timestamp}

## Active — ...       ← full entries with embedded stub blocks
## Paused             ← full entries or "None"
## Log                ← stub blocks only (warm queue, max 30)
```

---

## SESSION FILES

Session files are **ephemeral, branch-scoped** snapshots of in-progress work.

### Naming: `SESSION_{branch-slug}.md`
- Example: `SESSION_phase1-core-skeleton.md`
- Convention: strip `feat/` prefix from branch name

### Lifecycle:
- Created/overwritten at session start
- Updated every 15 minutes during work
- Stale after 2 hours of no `Last-Updated` change — next agent may ignore it

### Template (~20 lines max):
```markdown
# Session — {branch}
**Model:** {model-id} | **Started:** {ISO timestamp}
**Last-Updated:** {ISO timestamp}

## Current Step
{what you're doing right now}

## Files Being Modified
- {path}:{line-range} — {what}

## Background Tasks
- {task-id}: {description} — {status}

## Next Action
{what comes after current step}
```

### Why branch-scoped (not agent-scoped):
- Branch = work stream. Any agent can pick up where the last one left off.
- Parallel branches = parallel session files = no merge conflicts.
- "SESSION_sonnet.md" is ambiguous (which sonnet? which task?). "SESSION_ws5-local-infra.md" is clear.

---

## UPDATE RULES

**SESSION_{branch}.md:** Every 15 minutes while working
**WORK_TRACKER.md:** When starting task, completing steps, completing task
**MANIFEST.md:** When component status changes (not per-phase)

## STATE UPDATE RULES

Update these files explicitly when relevant:
- `WORK_TRACKER.md` when starting, pausing, resuming, or completing meaningful work
- `SESSION_{branch-slug}.md` during active work and before handoff
- `MANIFEST.md` only when project-level status, architecture, or known issues change

Do not assume shorthand requests imply these updates automatically unless the user explicitly asks for them.

## AI ATTRIBUTION (MANDATORY)

Every agent MUST self-identify in all artifacts:
- **Git commits** — add trailer: `AI-Model: {model-id}` (e.g., `claude-opus-4-6`, `codex`, `gemini-2.5-pro`)
- **WORK_TRACKER.md** — include `**Model:** {model-id}` field on every entry
- **PR descriptions** — include `**AI-Model:** {model-id}` line in summary

## CROSS-MODEL AWARENESS

Multiple AI models work on this repo. Before building on another agent's work:
1. Check `git log --oneline -10` for recent changes and who made them
2. If a key file changed significantly (>50% size change), investigate before continuing
3. Build-verify using the repo's standard verification command from `README.md` or package scripts after checkout/merge and before writing new code
4. If something looks wrong (missing UI, broken layout, unexpected state), check git history — don't silently "fix" what might be a regression from another agent

## MERGE CONFLICT RESOLUTION

- **NEVER** wholesale "keep theirs" or "keep ours" on files >50 lines
- Always review conflicts line-by-line and reconcile both sides
- Build-verify after every merge
- Document conflict resolution in commit message (what was kept from each side)

## CREDENTIALS SAFETY

- **NEVER** commit files containing plaintext secrets (API keys, client secrets, passwords)
- Check `git diff --cached` for secrets before committing
- If you find credentials in tracked files, warn the human — do not commit

## SUBAGENT / DELEGATION POLICY

When delegating to subagents, set the model or reasoning tier explicitly when the runtime supports it.

Use:
- small/fast models for file search, grep, simple reads, and test-output parsing
- medium models for implementation, debugging, and code review
- top-tier models for architecture, hard refactors, and ambiguous design problems

Rules:
- when unsure, use the middle tier first
- top-tier is allowed when the problem genuinely requires synthesis or hard design work
- do not use top-tier models for bulk file reads, simple grep, test running, or straightforward CRUD

## CRITICAL RULES

1. **NEVER modify the spec (`specs/`) unless the task is to change it** - Treat it as read-only by default
2. **ALWAYS write a task breakdown for non-trivial direct work** - Or it's lost on connection drop
3. **UPDATE SESSION file every 15 minutes while actively working** - Non-negotiable
4. **Read WORK_TRACKER.md at session start** - Continue from there
5. **NEVER put phase-level detail in MANIFEST** - Use plan files
6. **NEVER leave completed work in WORK_TRACKER** - Archive it monthly
7. **Do not preserve parallel obsolete architectures longer than necessary** - Delete superseded render paths after parity is verified
8. **NEVER hand-write metric names, labels or units from memory** - Read `specs/metric-contract.md` first; it is the authoritative source. A name or label that drifts from the contract silently breaks every dashboard and alert built on it.

---

# agent-quota-exporter: project context (read after the rules above)

## What this project is

A vendor-agnostic, read-only Prometheus exporter for AI coding-agent subscription quota windows and per-agent usage,
built for several accounts in several workspaces on one machine. It is an open-source project of its own and knows
nothing about any user's fleet. Status: pre-release, design phase (no implementation yet). The repo is private for
now; public timing is an open decision (`specs/decisions.md`).

## What lives where

| Path | What | Who may write it |
|---|---|---|
| `README.md` | overview, principles, safety notes, layout | maintainer, or an agent asked to |
| `specs/plan.md` | architecture, config, discovery, caching, errors, packaging, phases, Go vs Python recommendation | spec changes only |
| `specs/metric-contract.md` | metric names, types, units, labels, cardinality; versioned | spec changes only; state breaking or non-breaking per its Versioning section |
| `specs/sources.md` | per harness and provider: what is read, how, cadence, secret hazards | spec changes only |
| `specs/decisions.md` | open and settled decisions (language, licence, public timing, per-session metrics) | the maintainer decides; an agent records the ruling with date and who ruled |
| `fixtures/` | sanitized synthetic sample inputs; checks in `fixtures/README.md` | any task, after running those checks |
| `.aiworkflow/` | `WORK_TRACKER.md`, `MANIFEST.md`, `SESSION_*`, `rotate-work-tracker.py` | the agent doing the work |
| `.gitleaks.toml`, `.pre-commit-config.yaml`, `.github/workflows/secrets.yml` | secret scanning (hook plus CI backstop) | maintainer |

Later: the exporter source, tests, packaging, example dashboards and alert rules, all written generically.

## What never goes in this repo

- **Secrets**: no token, key, password, cookie or credential file content, real or example-looking. Fixtures use
  invented values only.
- **Fleet-specific config**: no real host names or IPs, account names or emails, inventories, graph ids, topology, or
  label-mapping tables of any particular user. Examples use `example.com` and neutral aliases (`node-a`, `acct-a1`).
- **Real telemetry or message content** from any agent: transcripts, prompts, tool output, session ids, paths beyond
  `/root` or `/home/<alias>`.
- Collected data, build output, venvs, local DB copies.

## Hard rules

- **Read-only toward agent state.** No code path may refresh, mint or write a token (`README.md` principles 3 and 5).
  Open SQLite with `mode=ro`; never take a lock that could block the owning agent.
- **Vendor-agnostic and fleet-agnostic.** Output labels are generic (`host`, `harness`, `profile`, `account`,
  `provider`, `model`, `window`); mapping them to anything else is the consumer's job.
- **Secrets never leave the file.** Read only inside the plugin that needs them, never logged, printed, put in an
  error message or a label. No message content: parse structural fields only.
- Before committing a fixture, run the checks in `fixtures/README.md`; they must return empty.
- Install the pre-commit hook once per checkout: `pip install pre-commit && pre-commit install`.

## Working mode

One workflow: direct work, with the ledgers above. Run `python .aiworkflow/rotate-work-tracker.py` at session start and
end; append your own `## Active` entry; never edit others' entries. A decision the maintainer takes goes into
`specs/decisions.md` the same session.

## Git

- Signed commits, conventional messages (`feat:`, `fix:`, `docs:`, `chore:`), trailer `AI-Model: <model-id>`.
- Do not commit or push unless the maintainer (or the orchestrating agent acting on the maintainer's instruction)
  says so. Pushing is deliberate, not automatic.
- Once the repo is public: `main` is protected, work goes on `feat/<slug>` or `docs/<slug>` branches, PRs are required
  and reviewed before merge, and a PR description carries `**AI-Model:** <model-id>`.
- No `git reset --hard`, force-push or rebase without the maintainer's say-so.
