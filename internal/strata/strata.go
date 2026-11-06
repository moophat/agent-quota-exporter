// Package strata reads lane availability from a Strata model server, without credentials.
package strata

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

type Source struct {
	Host, Lane, BaseURL string
	Interval            time.Duration
}

func (s *Source) Name() string              { return "strata" }
func (s *Source) Harness() string           { return "strata" }
func (s *Source) Profile() string           { return s.Lane }
func (s *Source) Network() bool             { return true }
func (s *Source) NetGroup() string          { return "strata/" + s.Lane }
func (s *Source) NetSpacing() time.Duration { return 5 * time.Second }
func (s *Source) MinInterval() time.Duration {
	if s.Interval < 5*time.Second {
		return 30 * time.Second
	}
	return s.Interval
}

type metrics struct {
	Live struct {
		Queued *uint64 `json:"queued"`
	} `json:"live"`
	Totals struct {
		Requests *uint64 `json:"requests"`
	} `json:"totals"`
	Engine struct {
		Model   *string `json:"model"`
		Images  *bool   `json:"images"`
		Context *uint64 `json:"max_context"`
	} `json:"engine"`
}

func (s *Source) read(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.BaseURL, "/")+path, nil)
	if err != nil {
		return core.Fail(core.ReasonOther)
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		var timeout net.Error
		if ctx.Err() != nil || (errors.As(err, &timeout) && timeout.Timeout()) {
			return core.Fail(core.ReasonTimeout)
		}
		return core.Fail(core.ReasonUpstream)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return &core.SourceError{Reason: core.ReasonRateLimited, Backoff: time.Minute}
	}
	if resp.StatusCode != http.StatusOK {
		return core.Fail(core.ReasonUpstream)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		var timeout net.Error
		if ctx.Err() != nil || (errors.As(err, &timeout) && timeout.Timeout()) {
			return core.Fail(core.ReasonTimeout)
		}
		return core.Fail(core.ReasonUpstream)
	}
	if len(b) > 1<<20 || json.Unmarshal(b, out) != nil {
		return core.Fail(core.ReasonParse)
	}
	return nil
}

func (s *Source) Collect(ctx context.Context, now time.Time, _ json.RawMessage) (core.Result, json.RawMessage, error) {
	var slots []struct {
		Processing *bool `json:"is_processing"`
	}
	var m metrics
	if err := s.read(ctx, "/slots", &slots); err != nil {
		return core.Result{}, nil, err
	}
	if slots == nil {
		return core.Result{}, nil, core.Fail(core.ReasonParse)
	}
	if err := s.read(ctx, "/metrics", &m); err != nil {
		return core.Result{}, nil, err
	}
	r := core.Result{ObservedAt: now}
	labels := []core.Label{{Name: "host", Value: s.Host}, {Name: "lane", Value: s.Lane}}
	add := func(name string, value float64) {
		r.Samples = append(r.Samples, core.Sample{Name: name, Labels: labels, Value: value})
	}
	busy, known := false, true
	for _, slot := range slots {
		if slot.Processing == nil {
			known = false
		} else if *slot.Processing {
			busy = true
		}
	}
	if busy {
		add("agent_lane_busy", 1)
	} else if known {
		add("agent_lane_busy", 0)
	}
	add("agent_lane_slots", float64(len(slots)))
	if m.Live.Queued != nil {
		add("agent_lane_queued", float64(*m.Live.Queued))
	}
	if m.Totals.Requests != nil {
		add("agent_lane_requests_total", float64(*m.Totals.Requests))
	}
	if m.Engine.Model != nil && len(*m.Engine.Model) <= 128 && m.Engine.Images != nil && m.Engine.Context != nil {
		info := append([]core.Label(nil), labels...)
		info = append(info, core.Label{Name: "model", Value: strings.ToLower(*m.Engine.Model)}, core.Label{Name: "images", Value: strconv.FormatBool(*m.Engine.Images)}, core.Label{Name: "context", Value: strconv.FormatUint(*m.Engine.Context, 10)})
		r.Samples = append(r.Samples, core.Sample{Name: "agent_lane_info", Labels: info, Value: 1})
	}
	return r, nil, nil
}
