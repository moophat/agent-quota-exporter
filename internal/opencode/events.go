package opencode

// events.go — emit allowlisted usage and collect events through the durable sink when it is configured.
//
// Contract: the event sink (events.Store) is optional. When nil, all functions in this file are no-ops
// and the plugin's metric behaviour is identical to its pre-B31 state. This satisfies the BRIEF requirement:
// "with events off, behaviour and metrics are unchanged."
//
// What is emitted:
//   - One usage event per complete assistant message, carrying the facts actually selected by this plugin:
//     message id, provider, model, token buckets, outcome, error_class, http_status, reported_cost, and
//     the message completion timestamp. Session, project, cwd, branch, skill, tier, speed are not selected
//     and remain null — they must not be guessed (spec §3.1 "Unknown project/session stays null").
//   - One collect event per Collect call that actually opened the database, capturing startup skip,
//     pending eviction and abandonment counts alongside the standard collect fields.
//
// Event-id stability: the hash covers (host, profile, harness, source, db-relative-path, table, row_id,
// revision), where revision is the SHA-256 of the allowlisted selected field values as written to the event.
// This matches the event-contract locator.database semantics. Replay of the same row is idempotent.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

const (
	parserVersion = "opencode-db/1"
)

// maybeNull returns a JSON null for empty strings (fields not selected/not observed).
func maybeNull(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage("null")
	}
	b, _ := json.Marshal(s)
	return b
}

// maybeNullInt returns null for 0, otherwise the integer.
func maybeNullInt(n int64) json.RawMessage {
	if n == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(fmt.Sprintf("%d", n))
}

// maybeNullFloat returns null for 0 or non-finite, otherwise the number.
func maybeNullFloat(f float64) json.RawMessage {
	if f == 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return json.RawMessage("null")
	}
	b, _ := json.Marshal(f)
	return b
}

// nowTS builds the two-field timestamp object for a known collection time.
func nowTS(t time.Time) json.RawMessage {
	ep := float64(t.Unix())
	b, _ := json.Marshal(struct {
		Original  *string `json:"original"`
		EpochSecs float64 `json:"epoch_seconds"`
		Quality   string  `json:"quality"`
	}{nil, ep, "collection"})
	return b
}

// sourceTS builds a timestamp object for a source-originated epoch (milliseconds in opencode).
func sourceTS(epochMillis int64) json.RawMessage {
	if epochMillis == 0 {
		return json.RawMessage(`{"original":null,"epoch_seconds":null,"quality":"unknown"}`)
	}
	ep := float64(epochMillis) / 1000.0
	src := fmt.Sprintf("%d", epochMillis)
	b, _ := json.Marshal(struct {
		Original  string  `json:"original"`
		EpochSecs float64 `json:"epoch_seconds"`
		Quality   string  `json:"quality"`
	}{src, ep, "source"})
	return b
}

// dbLocator builds the database locator for a message row.
func dbLocator(relPath, table string, rowID int64, revision string) json.RawMessage {
	gen := "0" // generation is the inode, but we use a stable placeholder; row identity covers it
	b, _ := json.Marshal(struct {
		Type     string `json:"type"`
		RelPath  string `json:"relative_path"`
		Gen      string `json:"generation"`
		Table    string `json:"table"`
		RowID    int64  `json:"row_id"`
		Revision string `json:"revision"`
	}{"database", relPath, gen, table, rowID, revision})
	return b
}

// collectLocator builds a probe locator for a collect-run event.
func collectLocator(runID string) json.RawMessage {
	b, _ := json.Marshal(struct {
		Type    string `json:"type"`
		ProbeID string `json:"probe_id"`
		Part    string `json:"part"`
	}{"probe", runID, "collect"})
	return b
}

// eventID computes a deterministic, stable event id from source identity and locator fields.
// The hash input is the concatenation of NUL-separated identity+locator components.
func eventID(host, profile, harness, source, relPath, table string, rowID int64, revision string) string {
	h := sha256.New()
	for _, s := range []string{host, profile, harness, source, relPath, table, fmt.Sprintf("%d", rowID), revision} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// collectEventID computes a deterministic event id for a collect run.
func collectEventID(host, profile, harness, source, runID string) string {
	h := sha256.New()
	for _, s := range []string{host, profile, harness, source, "collect", runID} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// usageRevision computes a revision fingerprint from the allowlisted selected fields of a message row.
// It covers the token buckets, outcome, error_class, http_status and cost — the fields that can change as
// opencode streams updates to the same row. A changed row is a new event.
func usageRevision(in, out, reasoning, cr, cw int64, errName string, status float64, cost float64) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\x00%d\x00%d\x00%d\x00%d\x00%s\x00%v\x00%v", in, out, reasoning, cr, cw, errName, status, cost)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// completedEpochMillis extracts the message completion time ($.time.completed) from the JSON row.
// opencode stores it as a Unix millisecond integer. Returns 0 if absent.
func completedEpochMillis(r []any) int64 {
	v := r[cCompleted]
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		if !math.IsNaN(x) && !math.IsInf(x, 0) && x > 0 && x < 1<<53 {
			return int64(x)
		}
	}
	return 0
}

// emittableMessage bundles the facts extracted from a completed message row to build one usage event.
type emittableMessage struct {
	messageID   string
	rowID       int64
	provider    string // already label-normalized
	model       string // already label-normalized (pre-cap/eviction label)
	in          int64
	out         int64
	reasoning   int64
	cacheRead   int64
	cacheW      int64
	errName     string  // empty if ok
	errStatus   float64 // HTTP status or 0
	cost        float64
	completedMs int64
}

// buildUsageEvent creates the JSONL envelope for one completed assistant message.
func (s *DBSource) buildUsageEvent(now time.Time, m emittableMessage) (json.RawMessage, error) {
	relPath := s.Path // the plugin already enforces no symlinks; path is absolute but stable per install

	revision := usageRevision(m.in, m.out, m.reasoning, m.cacheRead, m.cacheW, m.errName, m.errStatus, m.cost)
	eid := eventID(s.Host, s.Alias, Harness, SourceName, relPath, table, m.rowID, revision)

	outcome := "ok"
	var errClass json.RawMessage = json.RawMessage("null")
	var httpStatus json.RawMessage = json.RawMessage("null")
	if m.errName != "" {
		outcome = "error"
		ec := errCode(m.errName, m.errStatus)
		errClass, _ = json.Marshal(ec)
		if m.errStatus != 0 {
			httpStatus, _ = json.Marshal(m.errStatus)
		}
	}

	var reportedCost json.RawMessage = json.RawMessage("null")
	if m.cost > 0 {
		reportedCost, _ = json.Marshal(m.cost)
	}

	// raw_tokens: opencode provides disjoint buckets already; no normalization needed.
	// Per spec §3.1: "opencode buckets already are disjoint".
	rawTokens, _ := json.Marshal(map[string]json.RawMessage{
		"input":          maybeNullInt(m.in),
		"output":         maybeNullInt(m.out),
		"cache_read":     maybeNullInt(m.cacheRead),
		"cache_write":    maybeNullInt(m.cacheW),
		"cache_write_1h": json.RawMessage("null"),
		"reasoning":      maybeNullInt(m.reasoning),
		"total":          json.RawMessage("null"),
	})
	// tokens (normalized) = same as raw for opencode (already disjoint).
	tokens, _ := json.Marshal(map[string]json.RawMessage{
		"input":       maybeNullInt(m.in),
		"output":      maybeNullInt(m.out),
		"cache_read":  maybeNullInt(m.cacheRead),
		"cache_write": maybeNullInt(m.cacheW),
		"reasoning":   maybeNullInt(m.reasoning),
		"tok_cw_1h":   json.RawMessage("null"),
	})

	msgID := maybeNull(m.messageID)
	providerRaw := maybeNull(m.provider)
	modelRaw := maybeNull(m.model)
	outcomeRaw, _ := json.Marshal(outcome)

	data, _ := json.Marshal(map[string]json.RawMessage{
		"message_id":              msgID,
		"request_id":              json.RawMessage("null"),
		"message_at":              sourceTS(m.completedMs),
		"session_id":              json.RawMessage("null"), // not selected
		"parent_session_id":       json.RawMessage("null"),
		"agent_id":                json.RawMessage("null"),
		"sidechain":               json.RawMessage("null"),
		"cwd":                     json.RawMessage("null"), // not selected
		"project":                 json.RawMessage("null"),
		"project_mapping_version": json.RawMessage("null"),
		"branch":                  json.RawMessage("null"),
		"model":                   modelRaw,
		"harness_version":         json.RawMessage("null"),
		"entrypoint":              json.RawMessage("null"),
		"skill":                   json.RawMessage("null"),
		"tier":                    json.RawMessage("null"),
		"speed":                   json.RawMessage("null"),
		"account":                 json.RawMessage("null"), // not resolved at collection time
		"identity_interval_id":    json.RawMessage("null"),
		"account_evidence":        json.RawMessage("null"),
		"attribution_quality":     json.RawMessage(`"unattributed"`),
		"raw_tokens":              rawTokens,
		"tokens":                  tokens,
		"accounting_quality":      json.RawMessage("null"),
		"outcome":                 outcomeRaw,
		"error_class":             errClass,
		"http_status":             httpStatus,
		"source_status":           json.RawMessage("null"),
		"connection_id":           json.RawMessage("null"),
		"reported_cost":           reportedCost,
		"reported_cost_unit": maybeNull(func() string {
			if m.cost > 0 {
				return "usd"
			}
			return ""
		}()),
	})

	envelope := map[string]json.RawMessage{
		"schema_version":        json.RawMessage("1"),
		"kind":                  json.RawMessage(`"usage"`),
		"event_id":              json.RawMessage(`"` + eid + `"`),
		"source":                json.RawMessage(`"` + SourceName + `"`),
		"parser_version":        json.RawMessage(`"` + parserVersion + `"`),
		"host":                  json.RawMessage(`"` + s.Host + `"`),
		"profile":               json.RawMessage(`"` + s.Alias + `"`),
		"harness":               json.RawMessage(`"` + Harness + `"`),
		"source_schema_version": json.RawMessage("null"),
		"provider":              providerRaw,
		"observed_at":           sourceTS(m.completedMs),
		"collected_at":          nowTS(now),
		"locator":               dbLocator(relPath, table, m.rowID, revision),
		"quality":               json.RawMessage(`{"status":"observed","reasons":[],"missing_fields":["session_id","cwd","project","branch","tier","speed"]}`),
		"data":                  data,
	}
	return json.Marshal(envelope)
}

// runID derives a stable run identifier from the host, profile, source, and collection time (truncated to minute).
// This gives an idempotent id for a collect event: re-running within the same minute is idempotent.
func runID(host, profile, source string, now time.Time) string {
	h := sha256.New()
	ts := now.UTC().Truncate(time.Minute).Format("2006-01-02T15:04")
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s", host, profile, source, ts)
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// collectEnvelopeFields is the fixed set of fields we emit for a collect event from opencode.
type collectFields struct {
	startedAt        time.Time
	endedAt          time.Time
	filesRead        int64 // 1 if the DB was opened, 0 if not
	startupSkip      bool  // true on the very first collect run (pre-existing rows skipped)
	pendingEvicted   int64 // rows dropped due to pendingCap
	pendingAbandoned int64 // rows dropped due to pendingMaxAge
	inFlight         bool  // an assistant row is still pending completion
	completion       string
}

// buildCollectEvent creates the JSONL envelope for a collect run summary.
func (s *DBSource) buildCollectEvent(now time.Time, rid string, f collectFields) (json.RawMessage, error) {
	eid := collectEventID(s.Host, s.Alias, Harness, SourceName, rid)

	omissionReasons := []string{}
	if f.startupSkip {
		omissionReasons = append(omissionReasons, "startup_skip")
	}
	if f.inFlight {
		omissionReasons = append(omissionReasons, "in_flight_message")
	}
	omissionsRaw, _ := json.Marshal(omissionReasons)
	quality := json.RawMessage(`{"status":"observed","reasons":[],"missing_fields":[]}`)
	if f.inFlight {
		quality = json.RawMessage(`{"status":"partial","reasons":["in_flight_message"],"missing_fields":[]}`)
	}

	evicted := f.pendingEvicted + f.pendingAbandoned // total pending drops
	var filesRead json.RawMessage
	if f.filesRead > 0 {
		filesRead = json.RawMessage("1")
	} else {
		filesRead = json.RawMessage("0")
	}
	dur := f.endedAt.Sub(f.startedAt).Seconds()

	data, _ := json.Marshal(map[string]json.RawMessage{
		"run_id":                  json.RawMessage(`"` + rid + `"`),
		"started_at":              nowTS(f.startedAt),
		"ended_at":                nowTS(f.endedAt),
		"files_seen":              json.RawMessage("1"), // always 1 database
		"files_read":              filesRead,
		"bytes_read":              json.RawMessage("null"),
		"rows_appended":           json.RawMessage("null"),
		"backlog_bytes":           json.RawMessage("null"),
		"duration_seconds":        json.RawMessage(fmt.Sprintf("%f", dur)),
		"errors":                  json.RawMessage("null"),
		"first_error_class":       json.RawMessage("null"),
		"budget_exhausted":        json.RawMessage("null"),
		"deadline_exhausted":      json.RawMessage("null"),
		"skip_count":              json.RawMessage("null"),
		"replay_count":            json.RawMessage("null"),
		"reset_count":             json.RawMessage("null"),
		"evicted_count":           maybeNullInt(evicted),
		"abandoned_pending_count": maybeNullInt(f.pendingAbandoned),
		"completion":              json.RawMessage(`"` + f.completion + `"`),
		"coverage_start":          json.RawMessage(`{"original":null,"epoch_seconds":null,"quality":"unknown"}`),
		"coverage_end":            nowTS(f.endedAt),
		"omission_reasons":        omissionsRaw,
		"connections":             json.RawMessage("null"),
	})

	envelope := map[string]json.RawMessage{
		"schema_version":        json.RawMessage("1"),
		"kind":                  json.RawMessage(`"collect"`),
		"event_id":              json.RawMessage(`"` + eid + `"`),
		"source":                json.RawMessage(`"` + SourceName + `"`),
		"parser_version":        json.RawMessage(`"` + parserVersion + `"`),
		"host":                  json.RawMessage(`"` + s.Host + `"`),
		"profile":               json.RawMessage(`"` + s.Alias + `"`),
		"harness":               json.RawMessage(`"` + Harness + `"`),
		"source_schema_version": json.RawMessage("null"),
		"provider":              json.RawMessage("null"),
		"observed_at":           nowTS(f.endedAt),
		"collected_at":          nowTS(f.endedAt),
		"locator":               collectLocator(rid),
		"quality":               quality,
		"data":                  data,
	}
	return json.Marshal(envelope)
}
