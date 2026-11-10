# Changelog

## Unreleased

- Add the opt-in `zai-usage` source and the `zai` harness (`key_source: openclaw | opencode | zai-helper`, `quota: {zai_usage: true}`): z.ai GLM Coding Plan quota windows (`5h`, `7d`, `monthly` tool calls) and plan level, read with a key a local harness already holds. The exporter never asks for a key, never refreshes or writes anything, requests `api.z.ai` only, and reads a key shared by two profiles once. New gauges `agent_quota_used_calls` and `agent_quota_limit_calls`; `lite` joins the `plan` vocabulary. Contract version stays 0 (additive).
- Add the opt-in `codex-usage` source (`quota: {codex_usage: true}`): live Codex quota windows and credits from the ChatGPT backend, plus read-only reset-slot gauges `agent_quota_reset_credits_available` and `agent_quota_reset_credits_next_expiry_timestamp_seconds`. It never refreshes the token and never writes `auth.json`; the profile's `codex-rollouts` keeps its token counters and serves quota series only until `codex-usage` has a fresh reading.
- The scheduler offers free vendor-call slots oldest attempt first, so sources sharing a gate group take turns.
- Lower the `agy-usage` interval floor from 15 minutes to 2 minutes (the default stays 30 minutes); sources may now declare a shorter spacing between their vendor calls (`NetSpacer`), used by `agy-usage` (10 s) and `codex-usage` (5 s) so many accounts are all read within one interval.
- Read AgentKit's Claude status line cache for `/root/.claude` quota without vendor calls, with a per-profile cache path and optional OAuth fallback retaining its 15-minute floor.
