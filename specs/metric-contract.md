# Metric contract

Status: draft v0 (design phase). This is the interface between the exporter and everything that scrapes it. A builder
implements to this document; a consumer (dashboards, alerts, a label-mapping layer) relies on it. Breaking changes
bump the contract version (`agent_quota_exporter_build_info{contract="0"}`).

Retained structural facts use the optional [event-contract.md](event-contract.md), schema 1.
Metric cardinality limits apply to series, not to private event fields. Event support and the
diagnostics below are delivery requirements, not claims of runtime implementation.

Naming follows Prometheus conventions: `snake_case`, a unit suffix (`_seconds`, `_ratio`, `_total` for counters,
`_timestamp_seconds` for epoch times), base units, no unit in a label value. The prefix is `agent_` (generic, no
vendor). Names such as `llm_quota_utilization_ratio{provider,window,scope}` in prior art are borrowed **as an idea**
(ratio 0..1, window and scope as labels); the series here are defined independently.

## Label vocabulary

Labels are generic and defined once. A metric lists which of them it carries.

| Label | Meaning | Value rules |
|---|---|---|
| `host` | machine the exporter runs on | config `host` or the OS hostname; never an IP unless configured so |
| `harness` | the agent program: `claude-code`, `codex`, `opencode`, `agy`, ... | lowercase, fixed vocabulary per plugin |
| `profile` | one workspace: config dir / `CODEX_HOME` / opencode instance / agy `HOME` | operator alias from config; for auto-discovered profiles a stable slug of the directory name (not the path) |
| `account` | the login a profile holds | operator alias; default is a short stable hash of the account id. Public parity metrics use opaque identifiers, never raw emails; the existing email compatibility option is outside that privacy profile |
| `provider` | who serves the model: `anthropic`, `openai`, `google`, `opencode-go`, ... | as reported by the source; lowercased |
| `model` | model id as reported | verbatim, lowercased; unknown = `unknown` |
| `window` | quota window name: `5h`, `7d`, `weekly`, `monthly`, ... | normalised by the plugin (see below) |
| `scope` | what the window applies to: `all`, or a model family/limit kind such as `opus`, `sonnet` | `all` when the window covers everything |

`window` normalisation: durations as `5h`, `7d`, `30d`; named windows keep the vendor word lowercased (`weekly`);
`other` when the vendor reports a window without its length (the vendor's word is then the `scope`).
`scope` is never empty.

Operator-defined static labels (`labels:` in config, for example `env`, `team`) are appended to every series. They are
constant per exporter, so they add no cardinality. Total label count per series is capped (default 12).

## Cardinality rules

1. **No session id, request id, message id, pid, prompt or path as a label on any metric** in the default set.
   Project/cwd, branch, agent IDs, skill, timestamps and arbitrary limit names likewise stay out of
   high-volume labels. Observed structural IDs/tags/raw names are preserved privately in events
   before metric normalization; message content is excluded from both lanes.
2. **State is expressed as gauges, not as label churn.** A changing state (logged in, stale, active) is a 0/1 or count
   gauge with a fixed label set. An *info-style* gauge (constant value `1`, descriptive labels such as `plan`) is used
   for slow-changing facts, and its labels change only on real change.
3. Series per exporter is bounded by `hosts(1) x profiles x models x token types`. Model labels are capped per
   profile (default top 20 by recent use, the rest folded to `model="other"`).
4. Per-session series exist only behind `metrics.per_session: true` (default off) and are documented there as a
   cardinality hazard; they are not in the contract below.
5. Series for a profile that disappeared stop being emitted. Current-value eligibility follows the
   source freshness rules below; retained events and last-good evidence are never deleted because
   a series is absent. Missing means unknown, never zero.

## Quota metrics (gauges)

| Metric | Type | Unit | Labels | Meaning |
|---|---|---|---|---|
| `agent_quota_utilization_ratio` | gauge | ratio 0..1 (may exceed 1 only if the vendor reports >100%) | `host harness profile account provider window scope` | share of the window consumed, as reported by the vendor |
| `agent_quota_reset_timestamp_seconds` | gauge | Unix seconds | same | when the window resets; absent if the vendor gives none |
| `agent_quota_limit_info` | gauge (info) | constant 1 | same + `severity` | vendor-reported limit state, one series per active limit entry (`severity`: `normal`, `warning`, `critical`, ...). Only the current severity is exposed |
| `agent_quota_observed_timestamp_seconds` | gauge | Unix seconds | `host harness profile account provider` | when the vendor was last successfully asked (not when Prometheus scraped); basis for staleness alerts. For a passive source (below) it is the time of the reading in local state |
| `agent_quota_credits_available` | gauge | 0/1 | `host harness profile account provider scope` | 1 if the vendor reports credits left or unlimited, else 0. **0 is also what a plan with no credits in use reports**, so alert on a fall from 1 to 0, not on the value |
| `agent_quota_credits_balance` | gauge | the vendor's credit unit | same | balance as reported; absent if unlimited or not reported |
| `agent_quota_reset_credits_available` | gauge | slots | `host harness profile account provider` | reset slots (a vendor grant that clears a rate-limit window on request) the vendor says are usable now. Read only; the exporter never spends one. Emitted by `codex-usage` only |
| `agent_quota_reset_credits_next_expiry_timestamp_seconds` | gauge | Unix seconds | same | when the soonest-expiring unused reset slot expires. **Absent** when the vendor gives no expiry or no slot is open, never 0 |

Example:

```
# HELP agent_quota_utilization_ratio Share of a subscription quota window consumed, as reported by the vendor.
# TYPE agent_quota_utilization_ratio gauge
agent_quota_utilization_ratio{host="node-a",harness="claude-code",profile="work",account="acct-a1",provider="anthropic",window="5h",scope="all"} 0.42
agent_quota_utilization_ratio{host="node-a",harness="claude-code",profile="work",account="acct-a1",provider="anthropic",window="7d",scope="all"} 0.18
# TYPE agent_quota_reset_timestamp_seconds gauge
agent_quota_reset_timestamp_seconds{host="node-a",harness="claude-code",profile="work",account="acct-a1",provider="anthropic",window="5h",scope="all"} 1.793e+09
```

## Usage metrics (counters, from local agent state)

| Metric | Type | Unit | Labels | Meaning |
|---|---|---|---|---|
| `agent_tokens_total` | counter | tokens | `host harness profile account provider model type` | tokens consumed; `type` in `input`, `output`, `cache_read`, `cache_write`, `reasoning` |
| `agent_requests_total` | counter | requests | `host harness profile account provider model outcome` | model calls; `outcome` in `ok`, `error` |
| `agent_request_errors_total` | counter | errors | `host harness profile account provider model code` | calls that failed, `code` = a bounded class (`rate_limited`, `auth`, `overloaded`, `other`), never a raw message |
| `agent_cost_usd_total` | counter | USD | `host harness profile account provider model` | cost **as reported by the harness**, only where the source reports one; absent otherwise. Not a bill |

Counter semantics: the exporter keeps a durable offset per source in its state directory and counts only data it has
read since it first saw that source (a new profile starts at zero; optional `backfill_days` seeds history once).
After losing the state directory the counters restart at zero and Prometheus treats that as a counter reset; the
exporter never replays old data into a counter without `backfill_days`. `process_start_time_seconds` and
`agent_quota_exporter_state_epoch` expose restarts.

Historical event capture/import is independent of this live-counter start boundary. Replay adds
no live increments; account-switch corrections never decrement counters or move old usage to
the current login. Correctable event-time totals come from named event views, not counter increase().

Example:

```
# TYPE agent_tokens_total counter
agent_tokens_total{host="node-a",harness="claude-code",profile="work",account="acct-a1",provider="anthropic",model="model-x",type="input"} 1520
agent_tokens_total{host="node-a",harness="claude-code",profile="work",account="acct-a1",provider="anthropic",model="model-x",type="cache_read"} 98213
agent_requests_total{host="node-a",harness="opencode",profile="lane-1",account="acct-b1",provider="opencode-go",model="model-y",outcome="ok"} 311
```

## State metrics (gauges)

| Metric | Type | Unit | Labels | Meaning |
|---|---|---|---|---|
| `agent_account_logged_in` | gauge | 0/1 | `host harness profile account provider` | 1 if the profile holds a usable login (credential present and not known-expired), 0 otherwise |
| `agent_account_info` | gauge (info) | constant 1 | `host harness profile account provider plan` | slow-changing account facts; `plan` as reported, bounded to `pro`, `max`, `team`; any other or missing value is `unknown` |
| `agent_credential_expiry_timestamp_seconds` | gauge | Unix seconds | `host harness profile account provider` | access-token expiry time (a timestamp only, never the token); absent if unknown |
| `agent_sessions` | gauge | sessions | `host harness profile account state` | live sessions by state: `active`, `idle`, `other`. A count: no session ids |
| `agent_router_connection_enabled` | gauge | 0/1 | `host harness profile account provider` | 1 if `isActive=1`, else 0; distinguishes operator-disabled connections from failures; absent from legacy cached health until a successful database read |
| `agent_router_connection_up` | gauge | 0/1 | `host harness profile account provider` | a router's upstream connection (`account` = its alias or hash): 1 if active, its last test did not fail and it is not rate-limited now |
| `agent_router_rate_limited_until_timestamp_seconds` | gauge | Unix seconds | same | when the router's own rate limit on the connection ends; absent when none |

## Exporter health metrics

Every source (one plugin on one profile) reports its own health, so a broken source is visible without hiding the
healthy ones.

| Metric | Type | Unit | Labels | Meaning |
|---|---|---|---|---|
| `agent_source_up` | gauge | 0/1 | `host harness profile source` | 1 if the last attempt to read the source succeeded or a fresh cache entry exists |
| `agent_source_last_success_timestamp_seconds` | gauge | Unix seconds | same | last successful read |
| `agent_source_cache_age_seconds` | gauge | seconds | same | age of the value being served |
| `agent_source_backoff_until_timestamp_seconds` | gauge | Unix seconds | same | non-zero while a vendor 429 / error backoff is active |
| `agent_source_read_seconds` | gauge | seconds | same | duration of the last read |
| `agent_source_errors_total` | counter | errors | `host harness profile source reason` | read failures; `reason` in `not_found`, `permission`, `parse`, `timeout`, `auth_expired`, `rate_limited`, `upstream`, `other` |
| `agent_quota_exporter_build_info` | gauge (info) | constant 1 | `version contract` | build version and contract version |
| `agent_quota_exporter_profiles` | gauge | profiles | `harness origin` | profiles known; `origin` in `config`, `discovered` |
| `agent_quota_exporter_state_epoch` | gauge | Unix seconds | none | when the current persisted state was initialized: no `state.json` yet, or an invalid one that was set aside as `state.json.corrupt-<time>`; a change means the counters restarted from zero. A `state.json` that exists but cannot be read stops startup instead |
| `process_start_time_seconds` | gauge | Unix seconds | none | start time of the exporter process, taken as the binary initializes (before config loading) |
| `agent_collector_backlog_bytes` | gauge | bytes | `host harness profile source` | unread spans, including unvisited files |
| `agent_collector_last_complete_timestamp_seconds` | gauge | Unix seconds | same | last complete collection run |
| `agent_collector_files_seen` | gauge | files | same | files seen in latest run |
| `agent_collector_files_read` | gauge | files | same | files read in latest run |
| `agent_collector_bytes_read_total` | counter | bytes | same | bytes read, distinct from backlog |
| `agent_collector_rows_appended_total` | counter | rows | same | durably appended raw rows |
| `agent_collector_errors_total` | counter | errors | same + `class` | finite collector failure class |
| `agent_event_store_write_success` | gauge | 0/1 | `host harness profile source` | last durable event write succeeded; absent when event recording disabled |

New diagnostic `class` values: `timeout`, `network`, `unauthorized`, `forbidden`, `rate_limited`,
`invalid_data`, `unavailable`, `filesystem`, `budget`, `internal`. Existing source `reason`
labels retain their vocabulary. Exact per-run counts and numeric HTTP outcomes live in events.
These additions are non-breaking (contract remains 0); no existing family gains labels.

`source` is the plugin name (`claude-oauth-usage`, `claude-transcripts`, `codex-usage`, `opencode-db`, ...), a fixed vocabulary.

## Staleness and absence

- Active Claude quota is stale at age >=900 seconds or after reset without a post-reset reading.
  Current quota then is unknown, never synthesized zero. Last-good values/reset remain history;
  a failed read or fresh scrape cannot renew observed time. Other sources retain their documented
  staleness policies. Metric eligibility must not conceal readable historical evidence.
- **Passive sources are the one exception** (v0.2: `codex-rollouts`). They read a quota the vendor's own client wrote
  to disk at its last turn, so an idle profile's reading is old but still true. Such a window is served until its own
  `resets_at` passes, then goes absent; a window with no reset time falls back to `stale_after` from the reading. Credits
  are served until a newer reading replaces them, at most until the longest window of the same reading resets (7 days
  when the reading has none). `agent_quota_observed_timestamp_seconds` is the reading's own time, so the age alert above
  fires on any idle passive profile: for these, show the age ("as of") instead of alerting on it. While the files can
  be read, `agent_source_cache_age_seconds` stays near 0, because it measures the read, not the reading.
- A live source with a short interval (`codex-usage`, `agy-usage` when configured so) is stale after `stale_after` like
  any other; its `agent_quota_observed_timestamp_seconds` is the time of the last successful read, so
  `time() - agent_quota_observed_timestamp_seconds` is the real age of the quota. When the login's access token is
  expired (`codex-usage` never refreshes it) the last reading keeps being served with its true observed time while
  `agent_source_errors_total{reason="auth_expired"}` rises.
- `agent_source_up == 0` with `agent_source_errors_total{reason="auth_expired"}` rising means the real client has to
  refresh its login; the exporter does nothing about it by design.

## OTLP (optional)

Same metric names and attributes; counters map to monotonic sums, gauges to gauges, info gauges to gauges with value 1.
Units are carried in the OTLP unit field (`s`, `1`, `{token}`). Off by default.

## Versioning

The router enabled gauge is additive (contract stays 0). Existing connection-up semantics remain unchanged; alert on down connections only when `agent_router_connection_enabled == 1`.

`contract` in `agent_quota_exporter_build_info` is an integer. Adding a metric or a label value is non-breaking.
Removing or renaming a metric, changing a type, unit or label set is breaking and bumps it.
