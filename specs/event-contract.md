# Durable event contract

Status: schema 1, design contract for usage parity; not a claim of runtime implementation.
**AI-Model:** t9r/cx/gpt-6.1-sol

## Boundary and authority

An optional generic local event sink complements [metric-contract.md](metric-contract.md).
Metrics work without it; installations requiring retained history must enable it and treat failed
durable capture as incomplete coverage even if metrics work. No fleet graph, election, account chooser,
public event endpoint, deployment or credential mutation is introduced by this contract.
This contract supersedes the earlier metrics-only history boundary in `plan.md`, not Tu's recorded
rulings in [decisions.md](decisions.md). Retained facts and current metric eligibility are separate.

## Envelope (all kinds)

Every JSONL row is an object with these required keys. Nullable keys are present as JSON null,
never empty strings, zero, false or today's inferred value as a substitute for missing evidence.
Unknown numeric buckets remain null; observed zero is zero. Collections are empty only when the
source establishes an empty collection; otherwise null. No NaN or Infinity is valid JSON.

| Key | Type and semantics |
|---|---|
| `schema_version` | integer 1; envelope/payload version, independent of metric contract 0 |
| `kind` | `usage`, `identity`, `quota_observation`, `quota_attempt`, `collect`, `mirai_summary`, `mirai_request` |
| `event_id` | nonempty stable opaque identifier of a physical observation, not a logical message |
| `source`, `parser_version` | nonempty adapter name and version of interpretation |
| `source_schema_version` | original row schema/version as string, or null |
| `host`, `profile`, `harness` | nonempty configured generic identity; profile is an alias, not a directory |
| `provider` | original provider identifier or null; retain raw value before metric normalization |
| `observed_at`, `collected_at` | timestamp objects; source observation versus capture time |
| `locator` | object identifying the physical record, defined below |
| `quality` | object with `status` (`observed`, `partial`, `unknown`), bounded `reasons` array and `missing_fields` array of schema field names |
| `data` | kind-specific allowlisted object |

A timestamp object has `original` (source string/number or null), `epoch_seconds` (finite UTC Unix
seconds or null), and `quality` (`source`, `collection`, `unparseable`, `unknown`). Preserve original
source timestamps even when parsing fails. `collected_at` has a known epoch. A request with no source
clock uses read completion with quality `collection`; scanning an old file never renews its observation.
Import/mirror/generation timestamps are separate provenance, never replacements for either clock.

`locator` is a tagged object (`type`):

- `file`: `relative_path` within the configured source root, stable `file_id`, `generation`,
  `byte_start` inclusive and `byte_end` exclusive of the complete source line. Device/inode alone is
  not durable identity (reuse/truncation); persist generation and detect replacement/shrinkage.
- `database`: relative database identity, persisted database `generation`, `table`, `row_id`,
  `revision` fingerprint of the allowlisted selected fields. Re-reading an unchanged row is replay.
- `probe`: persisted unique `probe_id` allocated before an attempt and retained across append retry;
  `part` distinguishes attempt, identity and individual response products without copying a URL.
- `import`: stable artifact checksum, original row index/kind, optional original locator/event ID.
  Identical backup copies use the same artifact/row identity, independent of their import path.

Use a versioned deterministic hash over source identity (`host/profile/harness/source`), locator
identity and observation part to construct `event_id`. Exclude collection time, parser version,
transport path and metric labels from this hash. Persist file identity across restart; copied/archive
artifacts retain origin identity when proven, otherwise report possible duplicates. Different byte spans
or changed database revisions remain different events even for the same message. Reinterpretation
retains the ID and records its parser version in the derived view; it must not add tokens again.
Hash collision or same-ID/different-payload replay is an explicit integrity error, never last-write-wins.
Optional `provenance` contains only import artifact checksums, source pins, import epoch and original
event references; never arbitrary source objects. Cursors advance only after local append and fsync.

## Usage payload and accounting

`usage.data` contains nullable `message_id`, `request_id`, `message_at` (timestamp object),
`session_id`, `parent_session_id`, `agent_id`, `sidechain` (boolean), `cwd` (original directory),
`project`, `project_mapping_version`, `branch`, `model`, `harness_version`, `entrypoint`, `skill`,
`tier`, `speed`, `account`, `metric_account` (string or null), `identity_interval_id`, plus `account_evidence` and `attribution_quality`.
No tag is inferred from the current checkout. Project mapping is optional consumer configuration:
longest-prefix mapping, explicit worktree/repository aliases and overhead; a shared basename proves nothing.
Session key is `(host,profile,harness,session_id)`; cross-host continuation needs explicit linkage.

`raw_tokens` has nullable `input`, `output`, `cache_read`, `cache_write`, `cache_write_1h`,
`reasoning`, `total`. `tokens` has disjoint normalized `input`, `output`, `cache_read`,
`cache_write`, `reasoning`, plus `tok_cw_1h` (a subset of cache_write, never additive).
`accounting_quality` records missing buckets, invalid/negative arithmetic, subset violations,
source-reported totals and normalization basis; preserve raw numbers when normalization is uncertain.
`outcome`, `error_class`, `http_status`, `source_status`, `connection_id`, `reported_cost`,
`reported_cost_unit` are nullable observed structural fields for existing database adapters. Cost is
source evidence, never a quota unit or a price estimate. Error text/bodies are excluded.

- Claude display weight = input + cache_write + output; cache reads separate. Attribution weight =
  input + cache_write + 5*output; never apply this quota estimator to other providers.
- Codex raw input includes cached/write input; raw output includes reasoning. Normalize input by
  subtracting cached/write and output by subtracting reasoning, nonnegative and quality-checked.
  Reconstruct full output once, never add reasoning twice to raw output.
  Codex usage events also keep `raw_total_tokens`: the source's own cumulative token buckets
  (input, output, cache_read, cache_write, cache_write_1h, reasoning, total) as reported, so a
  total can be reconciled with the normalized buckets; nullable per bucket, never derived.
- opencode buckets already are disjoint; router input may include cached tokens with no separate
  breakdown. Missing router cache/reasoning buckets stay null, not invented zeros.

Raw revisions are retained indefinitely. Bounded dedupe sets cannot prove historical uniqueness.
Consumers explicitly select one of these views:

- `renderer-v1`: Claude globally by message ID, greatest output then latest collection time;
  retain earliest message time. Report cross-host/source collisions with both records. Other harnesses
  never merge into Claude IDs.
- `explorer-v2`: legacy Claude `(host,message_id)`; native `(host,profile,harness,message_id)`;
  earliest time/tags and per-token-field maxima. The root-profile legacy adapter may not silently
  collapse profiles: report collisions until a versioned profile-aware consumer is enabled.

Legacy nullable-cache-read-to-zero applies only inside the named compatibility view. Full imports,
archive deduplication and historical corrections never inflate or decrement current live counters.

## Identity payload and attribution

`identity.data`: `account` (public metric account label at observation or null), `metric_account`
(the same label, or null when unavailable), `identity_interval_id`,
`effective_from`, `effective_until` (nullable timestamp objects), optional `last_confirmed_at`, `credential_mtime`,
`access_expires_at`, `login_expires_at` (timestamp objects), `subscription_type`, `rate_tier`,
and restricted `metadata`. The next sample closes the preceding sampled binding; closure is a
derived interval, not a mutation of the raw event. No sample proves an exact switch time in a gap.

Non-breaking schema-1 extension (2026-11-07): `last_confirmed_at` is a timestamp
object recording the latest confirmation of the interval's unchanged facts.
Repeated confirmations append identity envelopes with distinct observation IDs,
the same `identity_interval_id` and `effective_from`, and advancing
`last_confirmed_at`; replay folds these by interval, taking the latest confirmation.
Older events without the field use `effective_from` as their last confirmation.
The gap after that confirmation and before a differing account stays ambiguous.

Claude's exact metadata allowlist is:

| Source field | Destination |
|---|---|
| `claudeAiOauth.expiresAt` | `access_expires_at` |
| `claudeAiOauth.refreshTokenExpiresAt` | `login_expires_at` |
| `claudeAiOauth.subscriptionType` | `subscription_type` |
| `claudeAiOauth.rateLimitTier` | `rate_tier` |
| `oauthAccount.emailAddress` | account label when `account_label: email` is configured; otherwise use the account identity policy below |
| `oauthAccount.organizationName`, `organizationType`, `billingType`, `seatTier` | corresponding exact keys in `metadata` |
| `oauthAccount.hasExtraUsageEnabled`, `organizationRateLimitTier`, `userRateLimitTier` | corresponding exact keys in `metadata` |
| credential filesystem mtime | `credential_mtime` |

No whole-object copy or guessed alias (`billingTier`, for example). Missing values stay null.
Billing/seat flags are metadata, not money presentation. Secrets and token claim blobs are excluded.
`account_evidence` retains only structural embedded account claim, sampled account and identity event
references, plus comparison (`match`, `conflict`, `sampled`, `none`). A reliable embedded claim is
compared, never silently overwritten. `attribution_quality` is `embedded`, `sampled`,
`sampled_legacy`, `ambiguous`, `conflict` or `unattributed`.
Claude transcript capture uses `account_evidence.comparison` values `session_owner`,
`identity_binding`, or `none`, with attribution quality `embedded`, `sampled`, or
`unattributed` respectively. A session-owner claim applies forward within its file;
without one, only a non-ambiguous IdentityAt binding at message time is eligible.
Schema-1 additive compatibility field: every new Claude usage event carries
`metric_account`, the same public account label as `account`. For `session_owner`,
it is `acct-` plus the first
eight lowercase hex digits of SHA-256 of `accountUuid`, or the profile's alias/email
label when the owner is the verified current login, exactly as `messageOwner`
resolves it. For `identity_binding`, it is the binding's metric account label;
for `none` or an `unknown` label, both fields are null. Configured email labels are
retained in both fields. Older usage events may omit `metric_account`.
Resolve by host **and profile** at message time, not the login when collected. Before first identity
is unattributed; events in an unobserved A-to-B gap are ambiguous, with separate totals. A late A
message is never assigned to current B by default. Legacy host-ASOF import is `sampled_legacy`,
with up-to-one-sample ambiguity. Later corrections affect event totals, not counter reassignment.

## Quota and collection payloads

`quota_observation.data` contains nullable `account`, `windows`, `limits`, `credits`, `reset_credits`,
and `normalization_quality`. Reader identity is the envelope host/profile; each window preserves
`name`, `scope`, `utilization_ratio`, `original_percent`, `reset_at` (timestamp), `duration_seconds`
and its own `observed_at`. Keep every numeric utilization window, including unknown names, before
metric caps/merging. `limits` preserves every reduced `{kind,percent,severity,is_active}` including
inactive and unknown kinds. Credit facts preserve availability/unlimited/balance/unit and reset-slot
count/expiries where actually read. No prices or monetary allowance amounts are added (D9).

`quota_attempt.data`: `account`, `started_at`, `ended_at`, `http_status` (integer, 0 for no HTTP
response), `success` (boolean), `failure_class` (null on success), `gate_until`, `cooldown_until`,
`gate_reason`, `probe_id`. Separate component probes preserve partial successes (such as reset slots).
Failure classes: `timeout`, `network`, `unauthorized`, `forbidden`, `rate_limited`, `invalid_data`,
`unavailable`, `filesystem`, `budget`, `internal`. Exact numeric status is retained, never status
text/body/header/credential-bearing URL. Failed attempts cannot overwrite successful observations.

`collect.data`: `run_id`, `started_at`, `ended_at`, `files_seen`, `files_read`, `bytes_read`,
`rows_appended`, `backlog_bytes`, `duration_seconds`, `errors`, `first_error_class`,
`budget_exhausted`, `deadline_exhausted`, `skip_count`, `replay_count`, `reset_count`,
`evicted_count`, `abandoned_pending_count`, `completion` (`complete`, `partial`, `failed`),
`coverage_start`, `coverage_end`, `omission_reasons`. Counts are nonnegative integers or null
when unknowable. Backlog includes unread spans of unvisited files; it is not bytes read. Scan caps,
startup omissions and in-flight omissions remain explicit. A bounded row budget leaves work queued.
Optional `connections` is an allowlisted array of observed `{connection_id,provider,is_active,
test_status,rate_limited_until}` health facts (not whole database rows); `test_status` is the structural
source enum, not an error message. Unknown values are flagged, never silently reported healthy.

Local cursor state, not event labels, stores device, inode, file generation, byte offset, file size,
overlong-line skip-through-newline state and durable append position. Skip/error counts enter collect
events. Separate identity and last-good quota snapshots are recoverable accelerators, not history.

## Source coverage

| Source | Per-message/request/session facts retained; known limits |
|---|---|
| Claude transcripts | All usage tags/buckets above where present in structural assistant records; full IDs, original timestamp, cwd/branch, version, entrypoint/skill, tier/speed and parent/sidechain linkage. Never content. |
| Claude sessions/account | Session source IDs/status/times only if actually observed; live counts require expiry/liveness, not permanent historic activity. Identity uses the exact allowlist above, no session-name text. |
| Claude passive/direct quota | All windows/limits and true payload time; direct requests emit attempt outcomes even on failure. No extra vendor call for events. |
| Codex rollouts | Raw/normalized last/total token usage, observed model and quota/credit facts. Retain structural session/request/message/parent/cwd/branch/version tags only when locally verified; otherwise null. Archive/full-history import is separate from bounded current-quota tail search. |
| Codex live usage | Existing opt-in usage and reset-slot GET facts/attempts; no message/session/project identity inferred. Real-response verification remains incomplete in sources.md. Unbuilt app-server/state readers are not activated. |
| opencode database | Existing selected message ID, rowid, role, provider/model, token buckets, cost, completion time, error name/status (name classified, never free text). Session/project/request tags not selected remain null. Record startup skip, pending eviction/24h abandonment and rowid-reuse coverage limitations. No new tables/columns. |
| 9router database | Existing usage row ID, connection ID, provider/model, prompt/completion tokens, cost and structural status; connection enabled/test/rate-limit facts become collection quality/health evidence. Missing original request time/session/project tags remain null; collection time is not request time. Preserve pre-cap identifiers, normalization and connection/scan-cap omissions. No credential/meta columns. |
| agy quota | Existing verified bucket IDs/windows/remaining fraction/reset and bounded attempt/guard outcomes; no request/session/project history is available. Existing CLI refresh conflict is unresolved, as recorded in sources.md. |
| Mirai manual import | Summary and request receipts below only, no periodic reads or new provider adapter. |

`mirai_summary.data`: `manual_observed_at`, `quota_total`, `quota_used`, `quota_remaining`,
`expires_at`, `requests_24h`, `success_count`, `error_count`, `success_rate`, `error_rate`,
`unit`, `unit_verified_non_monetary`, `unlimited`. Rates include their original scale/basis.
`mirai_request.data`: `manual_observed_at`, `request_at` (UTC timestamp), `request_id`, `model`,
`input_tokens`, `output_tokens`, `quota_charge`, `status`, `error_code`, `unit`,
`unit_verified_non_monetary`, nullable `account`, `project`, `session_id`. Import provenance keeps
requested `since`, pagination cursor/page and completeness, not provider URLs or response bodies.
Unknown units retain restricted original numeric evidence; display unknown until verified non-monetary.
Unlimited is explicit, never zero. Presentation loss requires maintainer resolution; no currency or
Claude-point conversion is assumed. Pagination and `--since` remain manual-reader/import responsibilities.

## Persistence, privacy and freshness

Store `events/month=YYYY-MM/<source-profile>.jsonl`, partitioned by UTC **write** month. Filename
components are validated identifiers, never account strings. One writer/file; O_APPEND, complete
JSON lines, torn-tail healing, fsync before cursor commit; directories 0750/files 0640. State uses
temp/fsync/rename. On append/fsync/capacity failure stop advancement, report backlog/store failure,
retry bounded runs. Local events survive transcript cleanup; no automatic deletion of raw events,
old shards/state or backups. Mirror/import is independent and idempotent; never append to old roots.
At least 90 days of daily queries plus all retained history via import/Explorer is required by parity;
metric-store retention is independent. Snapshot-only recovery cannot manufacture messages or add
aggregate snapshots to message totals. Preserve artifact checksums, import receipts and late-day reopening.

### Storage budget and recovery

Event recording requires a positive operator-configured local byte budget, a free-space reserve
(at least one bounded append batch plus state/checkpoint space), and a hot-history age budget
of at least 90 days. Count shards, state and local backups toward the byte budget. Warn at 80%
of that budget or when available filesystem space falls below twice the reserve. Before each
bounded batch, refuse writes that would exceed the budget or consume the reserve. Disk-full,
quota-exceeded and fsync failures follow the same failure path even below these thresholds.
The byte limit wins over the hot-age target: never silently delete evidence to meet either.

Expose pressure/size/free-space through the metric-contract store gauges; set
`agent_event_store_write_success` to 0 for affected sources and increment
`agent_collector_errors_total{class="filesystem"}` on each failed durable-write run.
These diagnostics must remain scrapeable from memory when the store itself cannot accept an
error event. Keep backlog and last-complete time truthful; an append failure is not completion.

The operator archives closed shards to separate storage, verifies checksums and a durable import
receipt before removing local copies, or moves/expands the store and its cursor state together.
Archive shards past the hot-age budget; archive earlier only when needed for the byte budget,
keeping at least 90 days queryable through the archive/import reader. Local backups need the
same verified offload before removal. Never remove active cursor/checkpoint state to free space.
If no verified archive is available, capture remains visibly blocked until capacity is restored.

Retry automatically on each normal bounded collection interval, without a restart or cursor reset.
After space returns, heal an incomplete tail, reconcile durable event IDs, and resume from the
last fsynced cursor; replay after a cursor-commit failure must not duplicate accounting. Catch up
every unread complete record still present in source transcripts, including rotated files tracked
by their persisted identities. Source records removed before catch-up cannot be recovered: report
an explicit coverage gap, never claim lossless capture for evidence no longer on disk.

No prompt/completion, tool arguments/results, environment dump, token, API key, cookie or arbitrary
JSON object enters events, derived storage, logs or UI. Paths/branch/IDs and restricted identity metadata
are private structural facts, never public metric dimensions. Reject secret-bearing structural strings
with a bounded reason rather than retaining them. Community examples/fixtures use invented neutral
values only. Configured account labels and identity `organizationName` may contain email
addresses; email shape alone is not a reason to redact or reject those fields.

### Account identity policy

Claude identity and usage events use the same public account label as metrics: the configured operator alias,
the source's existing `acct-xxxxxxxx` short stable hash, or email when `account_label: email`
is configured. For these events, `account` and `metric_account` are equal
when attribution is available; unavailable attribution remains null. Claude events do not introduce a separate account namespace.

Owner-authorized contract correction (Tu, 2026-11-08; see decisions.md): schema version
remains 1 despite the semantic change from private keyed account IDs to public metric labels.
`metric_account` remains additive compatibility data; readers tolerate its absence in older
schema-1 events. Existing retained rows are not rewritten or silently relinked; consumers
must distinguish old account semantics from newly captured public labels when reading mixed history.

### Claude freshness policy

Freshness never rewrites historical evidence. For active direct Claude quota, define `P` as the
effective configured account-wide poll interval (at least the source's floor), and `D` as the
bounded maximum normal scheduling/global-gate delay beyond that interval. Stale age is
`max(stale_after, 2*P + D + 15 seconds)`; stale means age greater than or equal to this bound.
The 15 seconds covers request completion. Gate coordination
must supply a finite delay bound, not an unbounded queue of holders. Failure/429 cooldowns do not
extend this age or renew observation time. Passive statusline readings retain their separate
short-cache freshness policy. Reset passed
without post-reset evidence means current unknown, prior reading retained. Identity target/local capture
is 60 s; health/collect ages remain separate. Passive Codex stays as-of its last real turn. Direct Claude
keeps >=900 s/account globally, 15 s request/no inline retry, account-wide >=3600 s after 429.
Generic external permits carry coordination; no fleet logic enters this repository.
Ordinary local/agy timeout/network/5xx/parse failures retry next interval, repeated failures 2x/4x
capped at 15 min with nonnegative jitter. Actual agy 429 keeps >=1 h; guard keeps 24 h. Auth changes
clear auth backoff only, not cooldown/gates; never refresh credentials. Missing files retry normally.
Corrupt network gates keep the existing one-hour quarantine; local history continues. Last-good facts
survive failures with original age. These are delivery policy requirements, not claims current code complies.

## L1 acceptance coverage

All original collected-fact IDs are accounted for; none is proposed for removal.

| ID | Contract home / explicit boundary |
|---|---|
| L1-001 | Envelope schema/source version, collected_at, host |
| L1-002 | usage message_id/request_id/message_at |
| L1-003 | usage session_id/agent_id/sidechain and parent linkage |
| L1-004 | usage cwd/project/mapping version/branch; private events, not metrics |
| L1-005 | usage model/harness_version/entrypoint/skill |
| L1-006 | usage tier/speed |
| L1-007 | raw_tokens and normalized tokens including tok_cw_1h subset |
| L1-008 | identity host/account/access_expires_at/login_expires_at/credential_mtime; account uses the configured public metric label, including email when configured |
| L1-009 | exact identity subscription/rate/organization/billing/seat/extra-usage allowlist |
| L1-010 | quota account/reader/observation and 5h/7d utilization/reset |
| L1-011 | complete raw windows and reduced limits, including unknown/inactive |
| L1-012 | quota_attempt time/account/host/numeric HTTP/success |
| L1-013 | collect files/bytes/backlog/rows/duration/errors/first class |
| L1-014 | local cursor device/inode/offset/size/overlong skip state; fsync ordering |
| L1-015 | monthly append-only events, separate identity/last-good snapshots, retained old roots |
| L1-016 | manual Mirai summary quota/expiry/24h counts/rates with original units/age |
| L1-017 | manual Mirai request time/model/tokens/charge/status/error; pagination/since provenance; unverified charge display remains pending |

Schema changes are additive only within version 1 when readers can safely ignore them. Changes to
meaning, required fields, deduplication or null semantics require a new schema version and compatible
consumer before activation. Unknown versions are unsupported coverage, never silently interpreted as v1.
The 2026-11-08 owner-authorized account-label correction above is an explicit exception
to the semantic-version rule, not a general permission for future semantic changes within v1.
