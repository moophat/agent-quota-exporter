# agent-quota-exporter

> Status: **pre-release, design phase.** No implementation exists yet. This repository holds the plan, the metric
> contract, the source catalogue and sanitized fixtures so a builder can start offline.

A read-only Prometheus exporter for AI coding agents. It reports, per machine, per workspace and per account:

- **subscription quota windows** (for example a 5-hour and a 7-day window, with utilization and reset time);
- **per-agent usage** (tokens, requests, errors, sessions) read from the agents' own local state.

It works across vendors and harnesses (Claude Code, Codex, opencode, agy and others, one plugin each) and is built
for the case where one machine runs **several accounts in several workspaces**.

## Why

People who run coding agents on subscriptions hit limits they cannot see coming, and teams that run many accounts
cannot tell which account is exhausted, expired or idle. The existing open-source exporters each cover a slice:

- some read local logs for token usage but have no quota windows and no multi-account model;
- some export quota windows, but do it by calling private vendor endpoints with CLI OAuth tokens and may refresh
  those tokens, which can revoke the live CLI session;
- billing exporters cover official API keys and admin APIs, not subscriptions.

No open-source project we found combines **subscription quota windows + per-agent usage, across vendors,
multi-account, without ever touching a token**. That is the gap. (Prior art and how it is used: see
[specs/plan.md](specs/plan.md#prior-art). Ideas are borrowed; no code is.)

## Principles

1. **Vendor-agnostic and fleet-agnostic.** The exporter knows nothing about any user's inventory, graph, host names or
   naming scheme. Output labels are generic: `host`, `harness`, `profile`, `account`, `provider`, `model`, `window`.
   Mapping those labels to anything else (a CMDB, a topology graph) is the consumer's job, done downstream.
2. **Multiple accounts in multiple workspaces on one machine is the norm.** A *profile* is one workspace: a Claude
   `CLAUDE_CONFIG_DIR`, a Codex `CODEX_HOME`, one opencode instance and its data dir, one agy `HOME`. One OAuth
   grant per profile, never copied between profiles.
3. **Read-only. Never refreshes or writes a token.** Refresh tokens are single-use: a second refresher (this
   exporter) would invalidate the grant the real client holds and log the user out. If an access token has expired,
   the exporter reports the profile as stale and waits for the real client to refresh it.
4. **Rate limits are per account, not per scrape.** Vendor usage endpoints lock out pollers. Every source has a
   minimum interval and a cache; Prometheus can scrape every 15 seconds and the vendor is still called at most once
   per interval per account, with backoff after a 429.
5. **Secrets never leave the file.** Tokens are read only inside the one plugin that needs them, held in memory for
   one request, and never logged, printed, put in an error message or a label. Credential files are opened only
   by plugins that declare it; everything else is allow-listed by column or key (see [specs/sources.md](specs/sources.md)).
6. **No message content, ever.** Transcripts, prompts and tool output are parsed for structural fields only
   (timestamps, model, usage numbers). Content fields are dropped at the parser.
7. **Prometheus first, OTLP optional.** `/metrics` is the contract ([specs/metric-contract.md](specs/metric-contract.md)).
   Cardinality is bounded by design: no session or request ids on hot series.
8. **Own config file, explicit workspaces, plus auto-discovery** of the standard locations.

## Safety notes

- The exporter needs read access to agent state directories and, for quota plugins only, to the credential file of
  each profile. Run it as the same user that owns the profiles, or give it a read-only group on those directories.
  It needs no write access to any of them (its own cache and offsets live in its own state dir).
- Opening a local SQLite database is done with `mode=ro` (read-only, `immutable` is not used on live databases).
  Never take a lock that could block the agent that owns the database.
- Quota plugins that call a vendor endpoint are **opt-in per profile** and flagged in the docs when the endpoint is
  private or undocumented. Anything that would need a token refresh is out of scope (or opt-in and marked risky).
- Fixtures in this repository are synthetic. Nothing in this repository is real telemetry.

## Layout

| Path | What |
|---|---|
| `specs/plan.md` | architecture, config, discovery, caching, errors, packaging, phases, Go vs Python recommendation |
| `specs/metric-contract.md` | metric names, types, units, labels, cardinality rules, examples |
| `specs/sources.md` | per harness and provider: what is read, how, cadence, secret hazards, out of scope |
| `specs/decisions.md` | open and settled decisions (licence locked; language, public timing, per-session open) |
| `fixtures/` | sanitized sample inputs for offline development (see `fixtures/README.md`) |
| `AGENTS.md` | rules for AI agents working in this repo (`CLAUDE.md` is a symlink to it) |
| `.aiworkflow/` | work ledger and component manifest |

## Licence

**Apache-2.0** (see `LICENSE`), the licence of node_exporter and the other prometheus/ exporters. Locked 2026-10-28; see
[specs/decisions.md](specs/decisions.md).
