package router

// events.go builds and emits allowlisted durable events for the router-9router source.
//
// What is emitted (specs/event-contract.md, "9router database"):
//   - One "usage" event per usageHistory row: row id, provider, model, connection id,
//     prompt/completion tokens, cost and structural status. No request time, session, project
//     or message id are available from this table; those fields remain null.
//   - One "collect" event per cycle: rows appended this cycle, backlog (rows remaining in
//     the table after the cycle's batch), bounded connection health facts, and error counts.
//
// Events are skipped when Source.EventStore is nil (events disabled).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/events"
)

const (
	parserVersion = "v1"
	eventSchema   = 1
)

// usageEvent builds one "usage" event from a usageHistory row.
// rowID is the database row id used for the locator and event_id.
// rawProvider, rawModel, connectionID and sourceStatus are the allowlisted source
// facts; provider, model and account are the current metric identities.
// in, out are token counts; ok is whether the request succeeded; cost is the reported cost.
// collectedAt is the cycle's wall clock.
func (s *Source) usageEvent(rowID int64, rawProvider, rawModel, connectionID, sourceStatus, provider, model, account string, in, out int64, ok bool, cost float64, collectedAt time.Time) json.RawMessage {
	// Deterministic event_id: sha256 of "source/host/profile/harness" + row id.
	idSrc := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d", SourceName, s.Host, s.Alias, Harness, rowID)
	h := sha256.Sum256([]byte(idSrc))
	eventID := hex.EncodeToString(h[:])

	outcome := "error"
	if ok {
		outcome = "ok"
	}
	var costVal any = cost

	collectedEpoch := float64(collectedAt.Unix())

	type tokenBuckets struct {
		Input  *int64 `json:"input"`
		Output *int64 `json:"output"`
	}
	inp, outp := in, out
	rawTok := tokenBuckets{Input: &inp, Output: &outp}

	type usageData struct {
		MessageID        any          `json:"message_id"`
		RequestID        any          `json:"request_id"`
		MessageAt        any          `json:"message_at"`
		SessionID        any          `json:"session_id"`
		ParentSessionID  any          `json:"parent_session_id"`
		AgentID          any          `json:"agent_id"`
		Sidechain        any          `json:"sidechain"`
		CWD              any          `json:"cwd"`
		Project          any          `json:"project"`
		ProjectMapVer    any          `json:"project_mapping_version"`
		Branch           any          `json:"branch"`
		Model            *string      `json:"model"`
		HarnessVersion   any          `json:"harness_version"`
		Entrypoint       any          `json:"entrypoint"`
		Skill            any          `json:"skill"`
		Tier             any          `json:"tier"`
		Speed            any          `json:"speed"`
		Account          *string      `json:"account"`
		IdentityInterval any          `json:"identity_interval_id"`
		AccountEvidence  any          `json:"account_evidence"`
		AttrQuality      any          `json:"attribution_quality"`
		RawTokens        tokenBuckets `json:"raw_tokens"`
		Tokens           any          `json:"tokens"`
		AccountingQual   any          `json:"accounting_quality"`
		Outcome          *string      `json:"outcome"`
		ErrorClass       any          `json:"error_class"`
		HTTPStatus       any          `json:"http_status"`
		SourceStatus     *string      `json:"source_status"`
		ConnectionID     *string      `json:"connection_id"`
		ReportedCost     any          `json:"reported_cost"`
		ReportedCostUnit any          `json:"reported_cost_unit"`
	}

	mdl := rawModel
	outc := outcome
	status := sourceStatus
	connection := connectionID
	data := usageData{
		Model:     &mdl,
		Account:   nil, // metric aliases/emails do not establish opaque account identity
		Outcome:   &outc,
		RawTokens: rawTok,
		// Tokens: normalized disjoint buckets; for router, input may include cache tokens with
		// no separate breakdown, so raw_tokens is what we have. tokens stays null to avoid
		// claiming normalization that wasn't done.
		Tokens:         nil,
		AccountingQual: nil,
		SourceStatus:   &status,
		ConnectionID:   &connection,
		ReportedCost:   costVal,
	}

	// Null timestamp object (no source timestamp available for router rows).
	nullTS := json.RawMessage(`{"original":null,"epoch_seconds":null,"quality":"unknown"}`)
	collTS, _ := json.Marshal(map[string]any{
		"original":      nil,
		"epoch_seconds": collectedEpoch,
		"quality":       "collection",
	})

	locator, _ := json.Marshal(map[string]any{
		"type":          "database",
		"relative_path": "db/data.sqlite",
		"generation":    nil,
		"table":         "usageHistory",
		"row_id":        rowID,
		"revision":      nil,
	})
	dataBytes, _ := json.Marshal(data)

	env := map[string]any{
		"schema_version":        eventSchema,
		"kind":                  "usage",
		"event_id":              eventID,
		"source":                SourceName,
		"parser_version":        parserVersion,
		"host":                  s.Host,
		"profile":               s.Alias,
		"harness":               Harness,
		"source_schema_version": nil,
		"provider":              rawProvider,
		"observed_at":           json.RawMessage(nullTS),
		"collected_at":          json.RawMessage(collTS),
		"locator":               json.RawMessage(locator),
		"quality": map[string]any{
			"status":         "partial",
			"reasons":        []string{"no_source_timestamp", "no_session", "no_project"},
			"missing_fields": []string{"message_id", "request_id", "message_at", "session_id", "cwd"},
		},
		"data": json.RawMessage(dataBytes),
	}
	raw, _ := json.Marshal(env)
	return json.RawMessage(raw)
}

// collectEvent builds one "collect" event summarising a Collect() cycle.
// rowsAppended is the count of rows read this cycle; backlogRows is the
// remaining rows past the cursor at the end of the cycle; errs is the per-class
// error count from the cycle; conns is the allowlisted health slice.
func (s *Source) collectEvent(
	runID string,
	started, ended time.Time,
	rowsAppended, backlogRows int64,
	errs map[string]int,
	conns []connFact,
) json.RawMessage {
	idSrc := fmt.Sprintf("collect\x00%s\x00%s\x00%s\x00%s", SourceName, s.Host, s.Alias, runID)
	h := sha256.Sum256([]byte(idSrc))
	eventID := hex.EncodeToString(h[:])

	startedEpoch := float64(started.UnixNano()) / 1e9
	endedEpoch := float64(ended.UnixNano()) / 1e9
	dur := ended.Sub(started).Seconds()

	errMap := map[string]int{}
	for k, v := range errs {
		errMap[k] = v
	}
	var firstErr *string
	// Aggregate counts do not establish observation order.

	startTS, _ := json.Marshal(map[string]any{
		"original": nil, "epoch_seconds": startedEpoch, "quality": "collection",
	})
	endTS, _ := json.Marshal(map[string]any{
		"original": nil, "epoch_seconds": endedEpoch, "quality": "collection",
	})
	collTS, _ := json.Marshal(map[string]any{
		"original": nil, "epoch_seconds": endedEpoch, "quality": "collection",
	})
	nullTS := json.RawMessage(`{"original":null,"epoch_seconds":null,"quality":"unknown"}`)

	locator, _ := json.Marshal(map[string]any{
		"type":     "probe",
		"probe_id": runID,
		"part":     "collect",
	})

	// Backlog in bytes is unknown; we know the row count. The collect event uses backlog_bytes
	// (nullable). We set it null and use rows_appended for the row count.
	// connections: allowlisted array of health facts.
	type connItem struct {
		ConnectionID     any `json:"connection_id"`
		Provider         any `json:"provider"`
		IsActive         any `json:"is_active"`
		TestStatus       any `json:"test_status"`
		RateLimitedUntil any `json:"rate_limited_until"`
	}
	connArr := make([]connItem, 0, len(conns))
	for _, c := range conns {
		var isActive any
		if c.enabled != nil {
			b := false
			if *c.enabled {
				b = true
			}
			isActive = b
		}
		var until any
		if c.until > 0 {
			until = c.until
		}
		connArr = append(connArr, connItem{
			ConnectionID:     c.id,
			Provider:         c.provider,
			IsActive:         isActive,
			TestStatus:       c.testStatus,
			RateLimitedUntil: until,
		})
	}

	completion := "complete"
	var omission []string
	if backlogRows > 0 {
		completion = "partial"
		omission = []string{"row_budget_exhausted"}
	}

	data := map[string]any{
		"run_id":                  runID,
		"started_at":              json.RawMessage(startTS),
		"ended_at":                json.RawMessage(endTS),
		"files_seen":              nil,
		"files_read":              nil,
		"bytes_read":              nil,
		"rows_appended":           rowsAppended,
		"backlog_bytes":           nil,
		"duration_seconds":        dur,
		"errors":                  float64(totalErrs(errMap)),
		"first_error_class":       firstErr,
		"budget_exhausted":        backlogRows > 0,
		"deadline_exhausted":      false,
		"skip_count":              nil,
		"replay_count":            nil,
		"reset_count":             nil,
		"evicted_count":           nil,
		"abandoned_pending_count": nil,
		"completion":              completion,
		"coverage_start":          nil,
		"coverage_end":            nil,
		"omission_reasons":        omission,
		"connections":             connArr,
	}
	dataBytes, _ := json.Marshal(data)

	env := map[string]any{
		"schema_version":        eventSchema,
		"kind":                  "collect",
		"event_id":              eventID,
		"source":                SourceName,
		"parser_version":        parserVersion,
		"host":                  s.Host,
		"profile":               s.Alias,
		"harness":               Harness,
		"source_schema_version": nil,
		"provider":              nil,
		"observed_at":           json.RawMessage(nullTS),
		"collected_at":          json.RawMessage(collTS),
		"locator":               json.RawMessage(locator),
		"quality": map[string]any{
			"status":         "observed",
			"reasons":        []string{},
			"missing_fields": []string{},
		},
		"data": json.RawMessage(dataBytes),
	}
	raw, _ := json.Marshal(env)
	return json.RawMessage(raw)
}

func totalErrs(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// connFact is an allowlisted health fact for the collect event's connections array.
type connFact struct {
	id         string
	provider   string
	enabled    *bool
	testStatus *string
	until      int64
}

// appendEvents sends the usage + collect batch, preserving original replay
// observations after a write or scheduler checkpoint failure.
// The caller must hold s.mu.
func (s *Source) appendEvents(store *events.Store, envs []json.RawMessage) error {
	if store == nil || len(envs) == 0 {
		return nil
	}
	originals, err := store.Originals(envs)
	if err != nil {
		return err
	}
	return store.Append(events.Batch{Envelopes: originals})
}

func collectedEvent(raw json.RawMessage, ended time.Time) json.RawMessage {
	var e map[string]any
	_ = json.Unmarshal(raw, &e)
	e["collected_at"].(map[string]any)["epoch_seconds"] = float64(ended.UnixNano()) / 1e9
	out, _ := json.Marshal(e)
	return out
}

func coverageEvent(raw json.RawMessage, startup bool, skipped int64) json.RawMessage {
	var e map[string]any
	_ = json.Unmarshal(raw, &e)
	d := e["data"].(map[string]any)
	var reasons []any
	if a, ok := d["omission_reasons"].([]any); ok {
		reasons = a
	}
	if startup {
		reasons = append(reasons, "startup_baseline")
	}
	if skipped > 0 {
		reasons = append(reasons, "malformed_rows")
	}
	d["skip_count"] = skipped
	if len(reasons) > 0 {
		d["completion"] = "partial"
		d["omission_reasons"] = reasons
	}
	out, _ := json.Marshal(e)
	return out
}
