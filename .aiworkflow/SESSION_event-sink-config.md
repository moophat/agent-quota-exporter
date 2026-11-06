# Session — feat/event-sink-config
**Model:** t9r/cx/gpt-6.1-sol
**Last-Updated:** 2026-11-06T23:00:00Z

## Current Step
B03 complete in working tree; no commit or push.

## Verification
Required config/app/events tests and additional CLI startup tests pass.
go vet ./..., race checks on affected packages, gofmt and diff checks pass.
Full go test -count=1 ./... fails agy TestClientRunsAsHomeOwner/65534 with permission error; isolated reproduction also fails. No agy changes.

## Decisions
Disabled by default; explicit positive byte allocation; reserve 8 MiB and hot age 90 days by default.
Write success remains absent before first append; unknown source backlog remains absent.
Capacity gauges are scrape-time; in-memory write outcomes survive capacity measurement failure.
Key outside store/backup boundary, mode 0600, owner-only, 32..4096 raw bytes.

## Next Action
Orchestrator reviews pending diff; B04+ supplies source event adapters and observed backlog.
