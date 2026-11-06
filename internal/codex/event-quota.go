package codex

import (
	"bufio"
	"encoding/json"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/events"
)

// boundedEventLine drains overlong records without retaining their contents.
func boundedEventLine(r *bufio.Reader) (string, int64, error) {
	var buf []byte
	var consumed int64
	overlong := false
	for {
		chunk, err := r.ReadSlice('\n')
		consumed += int64(len(chunk))
		if len(buf)+len(chunk) > maxLine {
			overlong = true
			buf = nil
		}
		if !overlong {
			buf = append(buf, chunk...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if overlong {
			return "", consumed, err
		}
		return string(buf), consumed, err
	}
}

func tokenFields(u map[string]*int64) map[string]any {
	out := map[string]any{"cache_write_1h": nil}
	for _, p := range [][2]string{{"input", "input_tokens"}, {"output", "output_tokens"}, {"cache_read", "cached_input_tokens"}, {"cache_write", "cache_write_input_tokens"}, {"reasoning", "reasoning_output_tokens"}, {"total", "total_tokens"}} {
		out[p[0]] = u[p[1]]
	}
	return out
}

func quotaEventData(q *rateLimits, timestamp string) map[string]any {
	var windows []any
	for _, p := range []struct {
		name string
		w    *rlWindow
	}{{"primary", q.Primary}, {"secondary", q.Secondary}} {
		if p.w == nil {
			continue
		}
		w := p.w
		var ratio, duration any
		if w.UsedPercent != nil {
			ratio = *w.UsedPercent / 100
		}
		if w.WindowMinutes != nil {
			duration = *w.WindowMinutes * 60
		}
		reset := map[string]any{"original": w.ResetsAt, "epoch_seconds": w.ResetsAt, "quality": "unknown"}
		if w.ResetsAt != nil {
			reset["quality"] = "source"
		}
		windows = append(windows, map[string]any{"name": p.name, "scope": q.LimitID, "original_percent": w.UsedPercent, "utilization_ratio": ratio, "duration_seconds": duration, "reset_at": reset, "observed_at": eventTime(timestamp)})
	}
	var credits any
	if q.Credits != nil {
		var balance any
		if n, ok := parseBalance(q.Credits.Balance); ok {
			balance = n
		}
		credits = map[string]any{"available": q.Credits.HasCredits, "unlimited": q.Credits.Unlimited, "balance": balance, "unit": nil}
	}
	return map[string]any{"account": nil, "windows": windows, "limits": nil, "credits": credits, "reset_credits": nil, "normalization_quality": "observed"}
}

func (s *RolloutsSource) appendQuota(c *eventFile, rel string, end int64, now time.Time, timestamp string, data map[string]any) error {
	structural, _ := json.Marshal(data)
	id := digest(s.Host + "/" + s.Alias + "/" + c.ID + "/quota/" + timestamp + "/" + string(structural))
	if s.eventSeen[id] {
		return nil
	}
	raw, err := json.Marshal(map[string]any{"schema_version": 1, "kind": "quota_observation", "event_id": id, "source": SourceName, "parser_version": "1", "source_schema_version": nil, "host": s.Host, "profile": s.Alias, "harness": Harness, "provider": Provider, "observed_at": eventTime(timestamp), "collected_at": map[string]any{"original": nil, "epoch_seconds": float64(now.UnixNano()) / 1e9, "quality": "collection"}, "locator": map[string]any{"type": "file", "relative_path": rel, "file_id": c.ID, "generation": c.ID, "byte_start": c.Offset, "byte_end": end}, "quality": map[string]any{"status": "partial", "reasons": []string{"unattributed"}, "missing_fields": []string{"account"}}, "data": data})
	if err != nil {
		return err
	}
	if err = s.Events.Append(events.Batch{Envelopes: []json.RawMessage{raw}}); err != nil {
		return err
	}
	s.eventSeen[id] = true
	return nil
}
