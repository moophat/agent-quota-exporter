# Work tracker

## Active

### 2026-10-28 03:38-03:55 +07 | claude-sonnet-5-5 | repo setup (docs, plan, contract, fixtures; no code)
Instruction: maintainer's explicit go, 2026-10-28 03:38 +07, relayed by the orchestrator.
- [x] README, LICENSE (Apache-2.0, provisional), .gitignore
- [x] docs/plan.md (architecture, config, discovery, caching, errors, packaging, phases, Go vs Python recommendation)
- [x] docs/metric-contract.md
- [x] docs/sources.md
- [x] fixtures/ (claude transcript/sessions/quota shape, opencode schema + rows, codex layout + rate-limits shape, agy note)
- [x] AGENTS.md, .aiworkflow/
- Verified: fixture secret/email greps return empty; opencode schema + rows load in sqlite3 :memory: (3 messages).
- Not verified: Codex app-server response shape and Codex state DB schema; agy (nothing on the node); whether a Codex
  rate-limit read can trigger a token refresh.
- Not done by design: no commit, no push, no implementation code.
- Open for the maintainer: licence (MIT vs Apache-2.0), language (Go recommended in plan, decision open), public timing.

## Log

(empty)

## Archive Index

(empty)
