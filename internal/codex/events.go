package codex

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/events"
	"github.com/moophat/agent-quota-exporter/internal/safefs"
)

// Event cursors are independent of the live metrics startup boundary.
type eventFile struct {
	Offset int64          `json:"offset"`
	Inode  uint64         `json:"inode"`
	ID     string         `json:"id"`
	Tags   map[string]any `json:"tags"`
}

func digest(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func eventTime(v string) map[string]any {
	t := map[string]any{"original": nullable(v), "epoch_seconds": nil, "quality": "unknown"}
	if v != "" {
		t["quality"] = "unparseable"
		if ts, err := time.Parse(time.RFC3339Nano, v); err == nil {
			t["epoch_seconds"] = float64(ts.UnixNano()) / 1e9
			t["quality"] = "source"
		}
	}
	return t
}

func (s *RolloutsSource) captureEvents(ctx context.Context, files []rollout, now time.Time) error {
	if s.eventSeen == nil {
		s.eventSeen = map[string]bool{}
		if err := s.Events.Replay(func(raw json.RawMessage) error {
			var e events.Envelope
			if err := json.Unmarshal(raw, &e); err != nil {
				return err
			}
			s.eventSeen[e.EventID] = true
			return nil
		}); err != nil {
			return err
		}
	}
	if s.st.Events == nil {
		s.st.Events = map[string]*eventFile{}
	}
	budget := int64(16 << 20)
	for _, file := range files {
		if budget <= 0 {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		key := hashKey(file.rel)
		cursor := s.st.Events[key]
		if cursor == nil || cursor.Inode != file.ino || file.size < cursor.Offset {
			cursor = &eventFile{Inode: file.ino, Tags: map[string]any{}}
			s.st.Events[key] = cursor
		}
		f, _, err := safefs.OpenRegular(file.path)
		if err != nil {
			return err
		}
		_, err = f.Seek(cursor.Offset, io.SeekStart)
		if err != nil {
			f.Close()
			return err
		}
		r := bufio.NewReaderSize(io.LimitReader(f, min(file.size-cursor.Offset, budget)), readBuf)
		for {
			if err := ctx.Err(); err != nil {
				f.Close()
				return err
			}
			line, consumed, err := boundedEventLine(r)
			budget -= consumed
			if err != nil {
				if err != io.EOF {
					f.Close()
					return err
				}
				break
			}
			end := cursor.Offset + consumed
			if cursor.ID == "" {
				cursor.ID = digest(line)
			}
			if line != "" {
				raw, id, err := s.rolloutEvent([]byte(line), cursor, file.rel, end, now)
				if err != nil {
					f.Close()
					return err
				}
				if raw != nil && !s.eventSeen[id] {
					if err := s.Events.Append(events.Batch{Envelopes: []json.RawMessage{raw}}); err != nil {
						f.Close()
						return err
					}
					s.eventSeen[id] = true
				}
			}
			cursor.Offset = end
		}
		f.Close()
	}
	return nil
}

func (s *RolloutsSource) rolloutEvent(line []byte, c *eventFile, rel string, end int64, now time.Time) (json.RawMessage, string, error) {
	var l struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Cwd     string `json:"cwd"`
			Model   string `json:"model"`
			Version string `json:"cli_version"`
			Info    *struct {
				Last  map[string]*int64 `json:"last_token_usage"`
				Total map[string]*int64 `json:"total_token_usage"`
			} `json:"info"`
			RateLimits *rateLimits `json:"rate_limits"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &l) != nil {
		return nil, "", nil
	}
	p := l.Payload
	if l.Type == "session_meta" {
		c.Tags["session_id"] = nullable(p.ID)
		c.Tags["cwd"] = nullable(p.Cwd)
		c.Tags["harness_version"] = nullable(p.Version)
	}
	if l.Type == "turn_context" {
		c.Tags["model"] = nullable(p.Model)
		if p.Cwd != "" {
			c.Tags["cwd"] = p.Cwd
		}
	}
	if l.Type != "event_msg" || p.Type != "token_count" {
		return nil, "", nil
	}
	if p.RateLimits != nil {
		d := quotaEventData(p.RateLimits, l.Timestamp)
		if err := s.appendQuota(c, rel, end, now, l.Timestamp, d); err != nil {
			return nil, "", err
		}
	}
	if p.Info == nil || p.Info.Last == nil && p.Info.Total == nil {
		return nil, "", nil
	}
	d := map[string]any{}
	for _, k := range strings.Fields("message_id request_id message_at session_id parent_session_id agent_id sidechain cwd project project_mapping_version branch model harness_version entrypoint skill tier speed account identity_interval_id outcome error_class http_status source_status connection_id reported_cost reported_cost_unit") {
		d[k] = nil
	}
	for k, v := range c.Tags {
		d[k] = v
	}
	d["message_at"] = eventTime(l.Timestamp)
	d["attribution_quality"] = "unattributed"
	d["account_evidence"] = map[string]any{"embedded_account": nil, "sampled_account": nil, "identity_event_id": nil, "comparison": "none"}
	raw, normalized := map[string]any{}, map[string]any{}
	missing := []string{}
	violations := []string{}
	for _, pair := range [][2]string{{"input", "input_tokens"}, {"output", "output_tokens"}, {"cache_read", "cached_input_tokens"}, {"cache_write", "cache_write_input_tokens"}, {"reasoning", "reasoning_output_tokens"}, {"total", "total_tokens"}} {
		v := p.Info.Last[pair[1]]
		raw[pair[0]] = v
		if v == nil {
			missing = append(missing, pair[0])
		}
		if pair[0] != "total" {
			normalized[pair[0]] = v
		}
	}
	raw["cache_write_1h"] = nil
	normalized["tok_cw_1h"] = nil
	for _, group := range [][]string{{"input", "cache_read", "cache_write"}, {"output", "reasoning"}} {
		var n int64
		valid := true
		for i, k := range group {
			v, _ := raw[k].(*int64)
			if v == nil {
				valid = false
				break
			}
			if *v < 0 {
				valid = false
			}
			if i == 0 {
				n = *v
			} else {
				n -= *v
			}
		}
		if n < 0 {
			valid = false
			violations = append(violations, group[0])
		}
		if valid {
			normalized[group[0]] = n
		} else {
			normalized[group[0]] = nil
		}
	}
	d["raw_tokens"] = raw
	if p.Info.Total != nil {
		d["raw_total_tokens"] = tokenFields(p.Info.Total)
	} else {
		d["raw_total_tokens"] = nil
	}
	d["tokens"] = normalized
	d["accounting_quality"] = map[string]any{"missing_buckets": missing, "subset_violations": violations, "source_reported_total": raw["total"], "normalization_basis": "codex-inclusive-input-output", "invalid_arithmetic": nil, "negative_arithmetic": nil}
	// Physical span plus structural payload distinguishes revisions, while copied rollouts retain identity.
	structural, _ := json.Marshal(d)
	id := digest(fmt.Sprintf("codex-event-v1/%s/%s/%s/%d/%d/%s", s.Host, s.Alias, c.ID, c.Offset, end, structural))
	e := map[string]any{"schema_version": 1, "kind": "usage", "event_id": id, "source": SourceName, "parser_version": "1", "source_schema_version": nil, "host": s.Host, "profile": s.Alias, "harness": Harness, "provider": Provider, "observed_at": eventTime(l.Timestamp), "collected_at": map[string]any{"original": nil, "epoch_seconds": float64(now.UnixNano()) / 1e9, "quality": "collection"}, "locator": map[string]any{"type": "file", "relative_path": rel, "file_id": c.ID, "generation": c.ID, "byte_start": c.Offset, "byte_end": end}, "quality": map[string]any{"status": "partial", "reasons": []string{"unattributed"}, "missing_fields": []string{"account", "project"}}, "data": d}
	b, err := json.Marshal(e)
	return b, id, err
}
