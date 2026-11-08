package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/events"
)

func captureTime(t time.Time) map[string]any {
	return map[string]any{"original": nil, "epoch_seconds": float64(t.UnixNano()) / 1e9, "quality": "collection"}
}
func sourceTime(v string) map[string]any {
	out := map[string]any{"original": nil, "epoch_seconds": nil, "quality": "unknown"}
	if v != "" {
		out["original"] = captureText(v)
		out["quality"] = "unparseable"
		if t, e := time.Parse(time.RFC3339Nano, v); e == nil {
			out["epoch_seconds"] = float64(t.UnixNano()) / 1e9
			out["quality"] = "source"
		}
	}
	return out
}

func captureText(v string) any {
	if v == "" {
		return nil
	}
	for _, marker := range []string{"bearer ", "sk-", "api_key=", "access_token", "refresh_token", "-----begin", "cookie:", "password=", "@"} {
		if strings.Contains(strings.ToLower(v), marker) {
			return nil
		}
	}
	return v
}
func (s *TranscriptsSource) captureEnvelope(kind, id string, locator, observed map[string]any, now time.Time, data map[string]any) map[string]any {
	return map[string]any{"schema_version": 1, "kind": kind, "event_id": captureDigest(s.P.Host + "\x00" + s.P.Alias + "\x00" + s.Name() + "\x00" + id), "source": s.Name(), "parser_version": "1", "source_schema_version": nil, "host": s.P.Host, "profile": s.P.Alias, "harness": Harness, "provider": Provider, "observed_at": observed, "collected_at": captureTime(now), "locator": locator, "quality": map[string]any{"status": "partial", "reasons": []string{}, "missing_fields": []string{}}, "data": data}
}

func captureDigest(identity string) string {
	h := sha256.Sum256([]byte(identity))
	return "v2:" + hex.EncodeToString(h[:])
}
func (s *TranscriptsSource) transcriptEvent(b []byte, rel string, c *transcriptCursor, start, end int64, now time.Time) (json.RawMessage, string, error) {
	transcriptOwner(b, &c.Owner)
	var l struct {
		Type       string `json:"type"`
		Timestamp  string `json:"timestamp"`
		Request    string `json:"requestId"`
		Session    string `json:"sessionId"`
		Parent     string `json:"parentSessionId"`
		Agent      string `json:"agentId"`
		Sidechain  *bool  `json:"isSidechain"`
		Cwd        string `json:"cwd"`
		Branch     string `json:"gitBranch"`
		Version    string `json:"version"`
		Entrypoint string `json:"entrypoint"`
		Skill      string `json:"skill"`
		Message    *struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				In    *int64 `json:"input_tokens"`
				Out   *int64 `json:"output_tokens"`
				Read  *int64 `json:"cache_read_input_tokens"`
				Write *int64 `json:"cache_creation_input_tokens"`
				Cache *struct {
					Hour *int64 `json:"ephemeral_1h_input_tokens"`
				} `json:"cache_creation"`
				Tier  string `json:"service_tier"`
				Speed string `json:"speed"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, "", err
	}
	if l.Type != "assistant" || l.Message == nil || l.Message.Usage == nil || l.Message.ID == "" || l.Message.Model == "<synthetic>" {
		return nil, "", nil
	}
	text := captureText
	u := l.Message.Usage
	var hour *int64
	if u.Cache != nil {
		hour = u.Cache.Hour
	}
	raw := map[string]any{"input": u.In, "output": u.Out, "cache_read": u.Read, "cache_write": u.Write, "cache_write_1h": hour, "reasoning": nil, "total": nil}
	tokens := map[string]any{"input": u.In, "output": u.Out, "cache_read": u.Read, "cache_write": u.Write, "tok_cw_1h": hour, "reasoning": nil}
	data := map[string]any{"message_id": l.Message.ID, "request_id": text(l.Request), "message_at": sourceTime(l.Timestamp), "session_id": text(l.Session), "parent_session_id": text(l.Parent), "agent_id": text(l.Agent), "sidechain": l.Sidechain, "cwd": text(l.Cwd), "project": nil, "project_mapping_version": nil, "branch": text(l.Branch), "model": text(l.Message.Model), "harness_version": text(l.Version), "entrypoint": text(l.Entrypoint), "skill": text(l.Skill), "tier": text(u.Tier), "speed": text(u.Speed), "account": nil, "identity_interval_id": nil, "account_evidence": map[string]any{"comparison": "none"}, "attribution_quality": "unattributed", "raw_tokens": raw, "tokens": tokens, "accounting_quality": map[string]any{"normalization_basis": "claude_disjoint"}}
	if l.Cwd != "" && !events.SafePath(l.Cwd) {
		data["cwd"] = nil
	}
	metric, account, interval, evidence, quality := s.messageOwner(l.Timestamp, c.Owner)
	data["metric_account"] = nil
	if evidence != "none" && metric != "unknown" {
		data["metric_account"] = metric
	}
	data["account"], data["identity_interval_id"] = account, interval
	data["account_evidence"], data["attribution_quality"] = map[string]any{"comparison": evidence}, quality
	data["message_id"] = text(l.Message.ID)
	negative := false
	for key, count := range map[string]*int64{"input": u.In, "output": u.Out, "cache_read": u.Read, "cache_write": u.Write, "tok_cw_1h": hour} {
		if count != nil && *count < 0 {
			tokens[key] = nil
			negative = true
		}
	}
	subset := hour != nil && u.Write != nil && *hour > *u.Write
	uncertain := hour != nil && u.Write == nil
	if subset || uncertain {
		tokens["tok_cw_1h"] = nil
	}
	if negative || subset || uncertain {
		// subset_violations names the violated subset fields (a list, as the event contract and Codex use);
		// a bool here made the store reject the envelope and stalled capture on that message.
		violations := []string{}
		if subset {
			violations = append(violations, "tok_cw_1h")
		}
		// Scalars in accounting_quality are strings or null (the event contract's scalar rule, as Codex emits).
		var invalid, negativeReason any
		if negative {
			negativeReason = "negative token count"
			invalid = "negative token count"
		}
		if subset {
			invalid = "tok_cw_1h exceeds cache_write"
		}
		data["accounting_quality"] = map[string]any{"normalization_basis": "claude_disjoint", "invalid_arithmetic": invalid, "negative_arithmetic": negativeReason, "subset_violations": violations}
	}
	// The store rejects secret-bearing structural strings; retain no arbitrary objects.
	locator := map[string]any{"type": "file", "relative_path": rel, "file_id": hashKey(fmt.Sprintf("%s:%d:%d", rel, c.Device, c.Inode)), "generation": c.Generation, "byte_start": start, "byte_end": end}
	id := fmt.Sprintf("%s:%d:%d:%d:%d:%d", rel, c.Device, c.Inode, c.Generation, start, end)
	env := s.captureEnvelope("usage", id, locator, sourceTime(l.Timestamp), now, data)
	missing := []string{}
	for _, key := range []string{"account", "project", "message_id", "request_id", "session_id", "model", "cwd", "branch"} {
		if data[key] == nil {
			missing = append(missing, key)
		}
	}
	reasons := []string{}
	if account == nil {
		reasons = append(reasons, "missing_attribution")
	}
	env["quality"] = map[string]any{"status": "partial", "reasons": reasons, "missing_fields": missing}
	encoded, err := json.Marshal(env)
	return encoded, env["event_id"].(string), err
}
