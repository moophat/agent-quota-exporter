# Contributing

Thanks for helping. Issues and pull requests are welcome; for a larger change (a new harness, a new metric family),
open an issue first so the design can be agreed before code is written.

## Build and test

Go 1.24 or newer.

```sh
go build ./cmd/agent-quota-exporter
go vet ./...
go test -race ./...
```

All three must pass. The golden exposition is `testdata/golden/metrics.txt`; after an intended change to the
output, run `go test ./internal/app -update` and review the diff.

Optional: install the pre-commit secret scan once per checkout with `pip install pre-commit && pre-commit install`
(it runs gitleaks with `.gitleaks.toml`; CI runs the same scan).

## Rules for changes

- **Read-only toward agent state.** No code path may refresh, mint or write a token, or write into an agent's
  directory. Open SQLite read-only and never take a lock that could block the agent that owns the database.
- **Secrets never leave the file.** A credential is read only inside the source that needs it, for one request, and
  never logged, printed, put in an error message or a label.
- **No message content.** Parse structural fields only; drop content fields at the parser.
- **The metric contract is authoritative.** Take metric names, labels and units from
  [specs/metric-contract.md](specs/metric-contract.md), not from memory, and update it in the same pull request.
  State whether a change is breaking (see its "Versioning" section) and add a line to [CHANGELOG.md](CHANGELOG.md).
- **Per-source details** (files, fields, endpoints, cadence, evidence level) belong in
  [specs/sources.md](specs/sources.md).
- **Vendor-agnostic and deployment-agnostic.** No real host names, IP addresses, account names, emails or
  inventories in code, tests, fixtures or docs. Use `example.com` and neutral aliases (`node-a`, `acct-a1`).
- **Fixtures are synthetic.** Every value is invented; a new fixture needs a provenance row in
  [fixtures/README.md](fixtures/README.md), and the checks listed there must print nothing.
- Keep changes focused, match the existing style, and add tests that would fail if the behaviour broke.

## Commits and pull requests

- Conventional commit messages (`feat:`, `fix:`, `docs:`, `test:`, `chore:`; `!` for a breaking change).
- A pull request describes what changed, why, and how it was tested.

By contributing you agree that your contribution is licensed under the [Apache-2.0](LICENSE) license.
