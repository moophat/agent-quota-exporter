# Session — feat/codex-events
**Model:** t9r/cx/gpt-6.1-sol | **Started:** 2026-11-07
**Last-Updated:** 2026-11-07

## Current Step
Requested continuation implemented and verified; raw_total_tokens contract naming concern remains.

## Files Being Modified
- internal/codex/events.go, events_test.go, rollouts.go — optional usage event capture
- internal/app/app.go — inject configured store
- internal/events/validation.go — numeric byte spans and source-reported total

## Verification
go test -count=1 ./internal/codex ./internal/app ./internal/events and focused go vet pass.

## Next Action
Review additive raw_total_tokens schema naming with contract owner; no commit or push. Dedicated archive directories/compression remain outside scope.
