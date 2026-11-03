# Cloud end-to-end run, 2026-11-03 (claude-opus-5-5)

`run.py` drives the real binary (built from `main` 7855aa7, `CGO_ENABLED=0`) against mocked Claude Code homes:
two accounts (a default `~/.claude` with the sibling `~/.claude.json`, and a `.claude-work` config dir), live
`sleep` processes as sessions plus a dead pid, 50 pre-existing history lines per profile, and a writer that appends
assistant lines across project and `subagents/` files, 30 % of them streamed twice with growing output.

```
go build -o /tmp/aqe ./cmd/agent-quota-exporter
python3 -I plans/261103-0900-exporter-v0.1-claude/itest/run.py /tmp/aqe "$(mktemp -d)"
```

Result (cloud container, real HTTP serving and scraping):

- first start with history present: nothing replayed;
- 30 series (2 profiles × 3 models × 4 token types + requests) equal the independently computed ground truth;
- restart mid-stream, with messages written while the exporter was down: converged again, nothing lost or doubled;
- counters monotonic across 25 scrapes;
- no content, name or token canary in `/metrics` or the log;
- sessions 1 active + 1 idle per profile, dead pid ignored; both profiles logged in; two distinct hashed account labels;
- profile trees (names, sizes, mtimes, sibling `.claude.json` included) exactly as the test wrote them, checked after
  each phase and after an idle cycle.

`run.py` refuses a non-empty directory: a previous run's state would not match a new run's ground truth.

The final scrape through agent-fleet-kit `mesh/label_map.py check` (main 706cca4, real fleet graph, host
`Ubuntu04-10-98-10-13`): the `root` profile's 36 series map to `harness/claude-code@U04`; the second profile's 36 are
reported unmapped (exit 1) until its Harness is declared; with it declared, 72 mapped, 0 unmapped.

Not covered here: the quota endpoint (its URL is not configurable by design; covered by the stub-server unit tests),
real Claude Code session status words beyond those mocked, systemd, and Prometheus/VictoriaMetrics ingestion.
