package app

import (
	"fmt"

	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/events"
)

func buildEvents(cfg *config.Config) (*events.Store, error) {
	if err := cfg.ValidateEvents(); err != nil {
		return nil, err
	}
	if !cfg.Events.Enabled {
		return nil, nil
	}
	s, err := events.Open(events.Options{Root: cfg.Events.RootDir, Budget: cfg.Events.ByteBudget, Reserve: cfg.Events.FreeSpaceReserve})
	if err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	if err := s.CheckWritable(); err != nil {
		s.Close()
		return nil, fmt.Errorf("events: startup write check: %w", err)
	}
	return s, nil
}

// EventSamples measures the sink independently of source cache freshness. Unknown
// source backlog and write outcomes remain absent until adapters report them.
func (b Built) EventSamples() []core.Sample {
	if b.Events == nil {
		return nil
	}
	st, err := b.Events.Stats()
	pressure := 0.0
	if st.Pressure || err != nil {
		pressure = 1
	}
	out := []core.Sample{}
	if err == nil {
		out = append(out,
			core.Sample{Name: "agent_event_store_size_bytes", Value: float64(st.Size)},
			core.Sample{Name: "agent_event_store_available_bytes", Value: float64(st.Available)},
		)
	}
	out = append(out, core.Sample{Name: "agent_event_store_pressure", Value: pressure})
	for i := range out {
		out[i].Labels = []core.Label{{Name: "host", Value: b.host}}
	}
	for w, ok := range b.Events.WriteOutcomes() {
		v := 0.0
		if ok {
			v = 1
		}
		out = append(out, core.Sample{Name: "agent_event_store_write_success", Value: v,
			Labels: []core.Label{{Name: "host", Value: w.Host}, {Name: "harness", Value: w.Harness},
				{Name: "profile", Value: w.Profile}, {Name: "source", Value: w.Source}}})
	}
	return out
}
