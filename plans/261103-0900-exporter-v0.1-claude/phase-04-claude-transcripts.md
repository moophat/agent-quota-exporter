---
phase: 4
title: Claude transcripts
status: done
depends: [2]
---

# Phase 04 — claude-transcripts

## Files owned
`internal/claude/transcripts.go` + `transcripts_test.go`.

## Plugin state (persisted JSON)
```
{ initialized bool,
  files: {relpath: {offset int64, size int64, inode uint64}},
  seen:  {msgID: {ts int64, in, out, cr, cw int64, model string}},   // 24 h horizon
  totals: {model: {in, out, cr, cw, req int64}},
  models: [first 20 distinct names] }
```

## Steps
1. Walk `dir/projects` (`filepath.WalkDir`, no symlink following) for `*.jsonl`. First run (`!initialized`):
   record offset=size for every file, emit totals (empty) and set initialized. A missing `projects/` dir is
   success with no files.
2. Per file: if inode changed or size < offset → offset 0. Open O_RDONLY, seek, `bufio.Reader` read complete
   lines only (stop at a final fragment without `\n`; do not advance past it). Line cap 16 MiB (longer → skip to
   next `\n`, count `parse`).
3. Per line: cheap pre-check `"type":"assistant"`; decode into a struct with only `type`, `timestamp`,
   `message.id`, `message.model`, `message.usage{input_tokens, output_tokens, cache_read_input_tokens,
   cache_creation_input_tokens}`. Skip `<synthetic>`, empty id, timestamp older than now−24h. Decode error →
   count `parse` (one counter increment per collect, not per line; no line content in logs).
4. Dedupe/delta (design §4.2): new id → add usage, req+1; existing id → add max(0, new−old) per field, store max.
   Model folding: name in `models` or len<20 → own name, else `other`.
5. Prune `seen` older than 24 h and `files` entries whose file vanished. Emit from `totals`:
   `agent_tokens_total{type}` (input/output/cache_read/cache_write) and `agent_requests_total{outcome="ok"}` with
   labels host/harness/profile/account/provider=anthropic/model.
6. Budget: stop reading after 64 MiB per collect (resume next tick) so a huge backlog cannot starve.

## Success criteria
- Fixture file read from 0 (initialized with file absent, then file appears): input 5, output 507 (412+95),
  cache_read 77647, cache_write 26495, requests 2, model-x only; `<synthetic>` absent.
- Restart with state → no double count; append a line → only it is counted; delete state → first run counts 0.
- Partial trailing line not consumed until completed; truncation resets offset without double count (dedupe).
- Canary in `message.content` never appears in samples, errors or logs.
