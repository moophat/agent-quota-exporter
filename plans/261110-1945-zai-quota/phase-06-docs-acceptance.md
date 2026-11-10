# Phase 6: specs, README, changelog, acceptance

## Files (owned)
`specs/sources.md`, `specs/metric-contract.md`, `specs/decisions.md` (only if the owner rules something; otherwise untouched), `README.md`,
`CHANGELOG.md`, `fixtures/README.md`, `examples/` (a commented z.ai profile snippet in the existing example config, if one exists).

## Steps
1. `specs/sources.md`: new section "z.ai (harness `zai`, provider `zai`)": table `zai-usage` (what, how, cadence, evidence), the key-source table
   with evidence levels as in plan.md (verified / documented / inferred / unverified marks kept), rules (opt-in, never refresh, one request, no proxy, region hosts,
   duplicate-key rule, failure reasons, mapping, unit inference, zcode unsupported and why). Edit the "Never-read" text of the opencode section to name the
   narrow `credential` exception for `zai`. Change the "Other providers" row "OpenRouter, DeepSeek, GLM ... not built" to say the GLM Coding Plan quota is
   built as `zai-usage` (quota windows only, no balance, D9 unaffected).
2. `specs/metric-contract.md`: add `zai` to the `harness` vocabulary, `zai` to `provider`, the two new quota gauges (table + unit), `lite` to the `plan` bound,
   `window=monthly` and `scope=tool_calls` examples, `source` vocabulary `zai-usage`. State contract stays 0 (additive).
3. `README.md`: short "z.ai" section: what is read, config example, the one-line "the exporter never asks for a key".
4. `CHANGELOG.md` Unreleased: add the source, new gauges, new harness.
5. `fixtures/README.md`: provenance rows for the `fixtures/zai/*` files (what was kept, what invented, canary convention); run both grep checks, they print nothing.
6. Whole-plan sweep: acceptance criteria 1-7 of `plan.md` checked one by one with the command that proves each; set `plan.md` status to `completed`.

## Validation
`go test ./... && go vet ./... && gofmt -l .` clean; fixtures grep checks print nothing; link check on edited markdown (every relative link resolves).
Hand the maintainer the live check (criterion 8) as the only unautomated step.

## Risk and rollback
Docs only. Revert.
