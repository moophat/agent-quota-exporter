# Q29 regression evidence
**Model:** t9r/cx/gpt-6.1-sol

Baseline: 7f5c1ef, source files unchanged before regression execution.
Command: `go test ./internal/router -run TestQ29 -count=1`.

| Finding | Regression | Baseline evidence |
|---|---|---|
| 1 | TestQ29EmailIdentity | FAIL: secret-bearing structural string |
| 2 | TestQ29RateLimit | FAIL: cannot unmarshal number into string |
| 3 | TestQ29Retry/sync, /state-save | Both FAIL: event ID 75e2… conflicting payload |
| 4 | TestQ29StorePressureMetrics | FAIL: samples=0 state=false; free-space reserve consumed |
| 5 | TestQ29ReusedRows | FAIL: events=1 unique=1, wanted two |
| 6 | TestQ29SparseBacklog | PASS before fix: finding rejected as stated; ordered exhausted scan reaches max ID even with gaps. Lookahead now proves remaining work directly. |
| 7 | TestQ29Duration | FAIL: zero duration |
| 8 | TestQ29StartupCoverage | FAIL: completion=complete, no startup omission |
| 9 | TestQ29OriginalHealth | FAIL: connection_id=null, test_status=active instead of testing |
| 10 | TestQ29ZeroCost | FAIL: observed zero cost lost |
| 11 | TestQ29MalformedCoverage | FAIL: completion=complete, skip_count=null |
| 12 | TestQ29FirstErrorUnknown | FAIL: aggregate map cannot prove first error |

Additional regression: TestQ29BudgetMetricsAndRecovery verifies budget rejection and recovery.

Acceptance (all exit 0):
```
go build ./...
# no output
go test -count=1 ./internal/router ./internal/app ./internal/events
ok github.com/moophat/agent-quota-exporter/internal/router 5.765s
ok github.com/moophat/agent-quota-exporter/internal/app 0.152s
ok github.com/moophat/agent-quota-exporter/internal/events 3.354s
go vet ./...
# no output
```

Design: metric cursor advances independently; one failed bounded event batch is retained in plugin state;
capture cursor advances only after append succeeds. Catch-up parses into a private state snapshot so
metric totals are never incremented twice. Existing stored envelopes supply original capture metadata
after restart/state-save failure. Usage source facts and locators must match before replay restoration.
Accounts stay null without proven opaque identity; metric compatibility labels remain independent.

Additional verification: gofmt check produced no output; `git diff --check` passed.
Focused race tests passed: router 47.075s; events 33.206s.
Not verified: live router, full repository race suite. No commit or push.
