package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Each object is closed: structural leaves never accept caller-supplied objects.
type shape map[string]string

type fieldError struct {
	field string
	err   error
}

func (e *fieldError) Error() string { return fmt.Sprintf("%s: %v", e.field, e.err) }
func (e *fieldError) Unwrap() error { return e.err }

func atField(field string, err error) error {
	var child *fieldError
	if errors.As(err, &child) {
		return &fieldError{field + "." + child.field, child.err}
	}
	return &fieldError{field, err}
}

func fields(keys, typ string) shape {
	s := shape{}
	for _, k := range strings.Fields(keys) {
		s[k] = typ
	}
	return s
}

var payloadKeys = map[string]string{
	"usage":             "message_id request_id message_at session_id parent_session_id agent_id sidechain cwd project project_mapping_version branch model harness_version entrypoint skill tier speed account identity_interval_id account_evidence attribution_quality raw_tokens tokens accounting_quality outcome error_class http_status source_status connection_id reported_cost reported_cost_unit",
	"identity":          "account metric_account identity_interval_id effective_from effective_until last_confirmed_at credential_mtime access_expires_at login_expires_at subscription_type rate_tier metadata",
	"quota_observation": "account windows limits credits reset_credits normalization_quality",
	"quota_attempt":     "account started_at ended_at http_status success failure_class gate_until cooldown_until gate_reason probe_id",
	"collect":           "run_id started_at ended_at files_seen files_read bytes_read rows_appended backlog_bytes duration_seconds errors first_error_class budget_exhausted deadline_exhausted skip_count replay_count reset_count evicted_count abandoned_pending_count completion coverage_start coverage_end omission_reasons connections",
	"mirai_summary":     "manual_observed_at quota_total quota_used quota_remaining expires_at requests_24h success_count error_count success_rate error_rate unit unit_verified_non_monetary unlimited",
	"mirai_request":     "manual_observed_at request_at request_id model input_tokens output_tokens quota_charge status error_code unit unit_verified_non_monetary account project session_id",
}
var nested = map[string]string{
	"raw_total_tokens":   "input output cache_read cache_write cache_write_1h reasoning total",
	"raw_tokens":         "input output cache_read cache_write cache_write_1h reasoning total",
	"tokens":             "input output cache_read cache_write reasoning tok_cw_1h",
	"metadata":           "organizationName organizationType billingType seatTier hasExtraUsageEnabled organizationRateLimitTier userRateLimitTier",
	"account_evidence":   "embedded_account sampled_account identity_event_id comparison",
	"accounting_quality": "missing_buckets invalid_arithmetic negative_arithmetic subset_violations source_reported_total normalization_basis",
	"windows":            "name scope utilization_ratio original_percent reset_at duration_seconds observed_at",
	"limits":             "kind percent severity is_active",
	"credits":            "available unlimited balance unit",
	"reset_credits":      "count expiries",
	"connections":        "connection_id provider is_active test_status rate_limited_until",
	"provenance":         "artifact_checksum source_pin import_epoch original_event_id original_locator since pagination_cursor page completeness",
}

func init() { payloadKeys["usage"] += " raw_total_tokens metric_account" }

func validateEnvelope(raw []byte, kind string) error {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return errors.New("invalid envelope object")
	}
	for _, k := range strings.Fields("schema_version kind event_id source parser_version host profile harness source_schema_version provider observed_at collected_at locator quality data") {
		if _, ok := obj[k]; !ok {
			return errors.New("missing required envelope field")
		}
	}
	allowed := fields("schema_version kind event_id source parser_version host profile harness source_schema_version provider observed_at collected_at locator quality data provenance", "scalar")
	for k, v := range obj {
		if _, ok := allowed[k]; !ok {
			return errors.New("unknown envelope field")
		}
		var err error
		switch k {
		case "observed_at", "collected_at":
			err = timestamp(v, k == "collected_at")
		case "locator":
			err = locator(v)
		case "quality":
			err = object(v, fields("status reasons missing_fields", "scalar"), true)
			if err == nil {
				var q struct {
					Status  string   `json:"status"`
					Reasons []string `json:"reasons"`
					Missing []string `json:"missing_fields"`
				}
				if json.Unmarshal(v, &q) != nil || (q.Status != "observed" && q.Status != "partial" && q.Status != "unknown") || len(q.Reasons) > 64 || len(q.Missing) > 128 {
					err = errors.New("invalid quality fields")
				}
			}
		case "data":
			err = object(v, fields(payloadKeys[kind], "scalar"), false)
		case "provenance":
			err = object(v, fields(nested[k], "scalar"), false)
		default:
			err = leaf(v)
			if k == "profile" {
				err = nil
			} // configured identity; never used as a filesystem component
			if k != "schema_version" {
				var text *string
				if json.Unmarshal(v, &text) != nil || (text == nil && k != "provider" && k != "source_schema_version") {
					err = errors.New("invalid envelope string")
				}
			}
		}
		if err != nil {
			return atField(k, err)
		}
	}
	return nil
}
func timestamp(v json.RawMessage, known bool) error {
	var m map[string]json.RawMessage
	if json.Unmarshal(v, &m) != nil || len(m) != 3 {
		return errors.New("invalid timestamp")
	}
	for _, k := range []string{"original", "epoch_seconds", "quality"} {
		if _, ok := m[k]; !ok {
			return errors.New("incomplete timestamp")
		}
	}
	var q string
	_ = json.Unmarshal(m["quality"], &q)
	if q != "source" && q != "collection" && q != "unparseable" && q != "unknown" {
		return errors.New("invalid timestamp quality")
	}
	var n *float64
	if json.Unmarshal(m["epoch_seconds"], &n) != nil || known && n == nil {
		return errors.New("invalid timestamp epoch")
	}
	return leaf(m["original"])
}
func locator(v json.RawMessage) error {
	var m map[string]json.RawMessage
	if json.Unmarshal(v, &m) != nil {
		return errors.New("invalid locator")
	}
	var typ string
	_ = json.Unmarshal(m["type"], &typ)
	keys := map[string]string{"file": "relative_path file_id generation byte_start byte_end", "database": "relative_path generation table row_id revision", "probe": "probe_id part", "import": "artifact_checksum row_index row_kind original_locator original_event_id"}[typ]
	if keys == "" {
		return errors.New("invalid locator type")
	}
	for _, k := range strings.Fields(keys) {
		if strings.HasPrefix(k, "original_") {
			continue
		}
		if _, ok := m[k]; !ok {
			return errors.New("incomplete locator")
		}
	}
	return object(v, fields("type "+keys, "scalar"), false)
}
func object(v json.RawMessage, allowed shape, required bool) error {
	var m map[string]json.RawMessage
	if json.Unmarshal(v, &m) != nil || m == nil {
		return errors.New("invalid structural object")
	}
	if required && len(m) != len(allowed) {
		return errors.New("incomplete structural object")
	}
	for k, val := range m {
		if _, ok := allowed[k]; !ok {
			return errors.New("unknown structural field")
		}
		if string(val) == "null" {
			continue
		}
		// File generations are opaque identities; router generations are numeric.
		if k == "generation" && (string(m["type"]) == `"file"` || (string(m["type"]) == `"database"` && len(val) > 0 && val[0] == '"')) {
			if err := leaf(val); err != nil {
				return err
			}
			continue
		}
		if k == "cwd" {
			var path string
			if json.Unmarshal(val, &path) != nil || !SafePath(path) {
				return errors.New("secret-bearing structural path")
			}
			continue
		}
		if k == "byte_start" || k == "byte_end" {
			var n int64
			if json.Unmarshal(val, &n) != nil || n < 0 {
				return errors.New("invalid byte offset")
			}
			continue
		}
		if strings.Contains(" source_reported_total input output cache_read cache_write cache_write_1h reasoning total tok_cw_1h input_tokens output_tokens files_seen files_read bytes_read rows_appended backlog_bytes duration_seconds errors skip_count replay_count reset_count evicted_count abandoned_pending_count http_status reported_cost utilization_ratio original_percent quota_total quota_used quota_remaining requests_24h success_count error_count quota_charge balance count percent rate_limited_until generation row_id ", " "+k+" ") {
			var number float64
			if json.Unmarshal(val, &number) != nil {
				return atField(k, errors.New("invalid numeric structural field"))
			}
			continue
		}
		if strings.Contains(" sidechain success budget_exhausted deadline_exhausted is_active available unlimited unit_verified_non_monetary hasExtraUsageEnabled ", " "+k+" ") {
			var flag bool
			if json.Unmarshal(val, &flag) != nil {
				return atField(k, errors.New("invalid boolean structural field"))
			}
			continue
		}
		var err error
		if keys, ok := nested[k]; ok {
			if k == "windows" || k == "limits" || k == "connections" {
				var a []json.RawMessage
				err = json.Unmarshal(val, &a)
				if err == nil {
					for _, x := range a {
						if err = object(x, fields(keys, "scalar"), false); err != nil {
							break
						}
					}
				}
			} else {
				err = object(val, fields(keys, "scalar"), false)
			}
		} else if strings.HasSuffix(k, "_at") || k == "effective_from" || k == "effective_until" || k == "credential_mtime" || k == "coverage_start" || k == "coverage_end" {
			err = timestamp(val, false)
		} else {
			if strings.Contains(" reasons missing_fields missing_buckets subset_violations omission_reasons expiries ", " "+k+" ") {
				var entries []json.RawMessage
				err = json.Unmarshal(val, &entries)
				if err == nil {
					for _, entry := range entries {
						if k == "expiries" {
							err = timestamp(entry, false)
						} else {
							var text string
							err = json.Unmarshal(entry, &text)
							if err == nil {
								err = leaf(entry)
							}
						}
						if err != nil {
							break
						}
					}
				}
			} else {
				var text string
				err = json.Unmarshal(val, &text)
				if err == nil {
					err = leaf(val)
					if k == "account" || k == "metric_account" {
						err = accountLeaf(val)
					} else if k == "model" || k == "organizationName" {
						err = modelLeaf(val)
					}
				}
			}
		}
		if err != nil {
			return atField(k, err)
		}
	}
	return nil
}

func modelLeaf(v json.RawMessage) error {
	var text string
	if json.Unmarshal(v, &text) != nil {
		return errors.New("invalid model")
	}
	// @ is a model version separator only in this field. All other secret checks remain.
	b, _ := json.Marshal(strings.ReplaceAll(text, "@", ""))
	return leaf(b)
}

// Account labels may contain email addresses and incidental marker substrings.
// Explicit credential assignments and recognizable keys remain forbidden.
func accountLeaf(v json.RawMessage) error {
	var text string
	if json.Unmarshal(v, &text) != nil || pathSecret.MatchString(text) {
		return errors.New("secret-bearing account string")
	}
	return nil
}

var pathSecret = regexp.MustCompile(`(?i)(bearer\s|sk-[a-z0-9_-]{20,}|api_key=|access_token[=:]|refresh_token[=:]|-----begin|cookie:|password=)`)

// SafePath permits ordinary directory names, including scoped packages and task-*,
// while rejecting explicit credential assignments and recognizable key material.
func SafePath(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsAny(path, "\x00\r\n") && !pathSecret.MatchString(path)
}
func leaf(v json.RawMessage) error {
	var x any
	if json.Unmarshal(v, &x) != nil {
		return errors.New("invalid structural value")
	}
	switch x := x.(type) {
	case map[string]any:
		return errors.New("unexpected structural object")
	case []any:
		for _, e := range x {
			b, _ := json.Marshal(e)
			if err := leaf(b); err != nil {
				return err
			}
		}
	case string:
		lower := strings.ToLower(x)
		for _, marker := range []string{"bearer ", "sk-", "api_key=", "access_token", "refresh_token", "-----begin", "cookie:", "password=", "@"} {
			if strings.Contains(lower, marker) {
				return errors.New("secret-bearing structural string")
			}
		}
	}
	return nil
}
