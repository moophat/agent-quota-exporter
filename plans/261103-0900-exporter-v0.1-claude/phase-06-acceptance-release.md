---
phase: 6
title: Acceptance, CI, release, ledgers, PR
status: done
depends: [1, 2, 3, 4, 5]
---

# Phase 06 — acceptance, CI, release

## Files owned
`internal/e2e_test.go` (or `cmd/.../main_test.go`), `testdata/golden/metrics.txt`, `.github/workflows/{ci.yml,release.yml}`,
`examples/agent-quota-exporter.service`, `README.md` (status + usage section only), `.aiworkflow/{WORK_TRACKER.md,MANIFEST.md,SESSION_phase1-core.md}`.

## Steps
1. E2E test builds a temp tree with two Claude profiles from fixtures, fixed clock, stub quota server, runs one
   full collect, renders → compares with `testdata/golden/metrics.txt` (`-update` flag regenerates).
2. Contract test: parse `specs/metric-contract.md` tables for metric names; every emitted family must be listed.
3. Canary + read-only snapshot tests over the full run (design §5).
4. CI `ci.yml`: push/PR → setup-go 1.24, `go vet`, `go test -race -count=1 ./...`, fixture greps.
5. `release.yml`: on tag `v*` → test, build `CGO_ENABLED=0` linux/amd64 + linux/arm64 with
   `-ldflags "-s -w -X main.version=${tag}"`, tar.gz each with LICENSE+README+examples, `SHA256SUMS`, create GitHub
   release (prerelease if tag contains `-`) via `gh release create` with `GITHUB_TOKEN` (`contents: write`).
6. Example systemd unit (hardening per spec §Security posture, runs as root by default comment: read access to
   all homes needed) + example config.
7. Ledgers: WORK_TRACKER Active entry with `**Model:**`, SESSION file, MANIFEST component statuses.
8. Commit per phase (signed, conventional, `AI-Model: claude-opus-5-5`), push, open PR with `**AI-Model:**`, tag
   `v0.1.0-rc.1` on PR head, check release run.

## Success criteria
- `go vet ./... && go test -race -count=1 ./...` green locally and in CI.
- Release run for `v0.1.0-rc.1` publishes 2 tarballs + SHA256SUMS; `sha256sum -c` passes on a downloaded copy.
- `--once` against the fixture tree prints the golden output.
