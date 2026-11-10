# Phase 2: key reader seam, `openclaw` and `zai-helper`

## Context
`plan.md` "Key sources". Pattern: `internal/codex/usage.go` `readAuth` (safefs, size cap, zeroed buffer). No network here.

## Files (owned, all new)
`internal/zai/keysource.go`, `openclaw.go`, `helper.go`, `keysource_test.go`, `openclaw_test.go`, `helper_test.go`,
`fixtures/zai/openclaw.json5`, `fixtures/zai/chelper-config.yaml` (+ provenance rows in `fixtures/README.md` at phase 6, but add the
rows now so the fixture rule holds).

## Interface (keep minimal)
```go
// key is request-local; Zero wipes it.
type key struct{ value []byte; region string /* "global"|"cn"|"" hint */; fp string /* mtime:size of the file */ }
func (k *key) Zero()
type reader func(dir string) (key, error) // errors are core.Fail(reason) only
func readerFor(source string) (reader, bool)
```

## Tests first
Fixtures use canary values that pass the key shape check but match no vendor prefix (e.g. `CANARYZAI0000000000000000.CANARYZAI00000000`);
the fixtures grep checks in `fixtures/README.md` must still print nothing.
1. openclaw: fixture with `//` comment lines, trailing commas, `models.providers.zai.apiKey` -> key, region from `baseUrl` host
   (`api.z.ai` global, `open.bigmodel.cn` cn, other/absent host -> ""); only `zai-anthropic.apiKey` present -> used; neither but
   `env.vars.ZAI_API_KEY` -> used; apiKey is an object (`{source:"env",...}`) -> `parse`; empty file -> `not_found`; both `zai` and
   `zai-anthropic` with different values -> `zai` wins.
2. openclaw JSON5-lite: `//` and `/* */` comments inside and outside strings (a `//` inside the baseUrl string must survive),
   trailing commas, BOM; single-quoted strings or unquoted keys -> `parse` (documented limit).
3. zai-helper: `api_key` + `plan: glm_coding_plan_china` -> region `cn`; `glm_coding_plan_global` -> `global`; missing `api_key` ->
   `not_found`; `api_key` not a string -> `parse`.
4. Common: missing dir -> `not_found`; dir or file is a symlink -> `permission`; FIFO/dir named `openclaw.json` -> `permission`
   (safefs behaviour); file over 1 MiB -> `parse`; key with space, CR/LF, non-ASCII, <16 or >512 bytes -> `parse`; `fp` changes when
   the file is rewritten.
5. No error string from any reader contains the key or the path (assert on `err.Error()`).
6. Buffer wipe: after `Zero`, `value` is all zero bytes.

## Steps
1. `keysource.go`: shape check, `Zero`, `readerFor`, a shared `openHolderFile(dir, name)` (safefs) returning bytes.
2. `openclaw.go`: strip comments/trailing commas with a small state machine that respects strings, `encoding/json` into a struct with
   `json.RawMessage` for `apiKey`, accept only a JSON string.
3. `helper.go`: yaml.v3 into a struct with `any` for `api_key`.

## Validation
`go test ./internal/zai/...` and `go vet ./...`.

## Risk and rollback
JSON5 breadth: real OpenClaw files may use syntax outside the subset; failure is a clean `parse`, documented. New package only; delete it to roll back.
