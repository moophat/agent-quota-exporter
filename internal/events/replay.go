package events

import (
	"encoding/json"
	"errors"
	"reflect"
)

// Originals restores the first observation envelope on source replay. Only
// capture metadata may differ; changed source facts remain integrity errors.
// Append still performs the durability reconciliation and strict digest check.
func (s *Store) Originals(envs []json.RawMessage) ([]json.RawMessage, error) {
	wanted := map[string]int{}
	out := append([]json.RawMessage(nil), envs...)
	for i, raw := range envs {
		var e Envelope
		if json.Unmarshal(raw, &e) != nil {
			return nil, errors.New("invalid replay envelope")
		}
		ok, err := s.Contains(e.EventID)
		if err != nil {
			return nil, err
		}
		if ok {
			wanted[e.EventID] = i
		}
	}
	if len(wanted) == 0 {
		return out, nil
	}
	err := s.Replay(func(raw json.RawMessage) error {
		var e Envelope
		_ = json.Unmarshal(raw, &e)
		i, ok := wanted[e.EventID]
		if !ok {
			return nil
		}
		var old, next map[string]any
		_ = json.Unmarshal(raw, &old)
		_ = json.Unmarshal(envs[i], &next)
		if e.Kind == "usage" && (!reflect.DeepEqual(old["data"], next["data"]) || !reflect.DeepEqual(old["locator"], next["locator"])) {
			return errors.New("event replay source facts conflict")
		}
		out[i] = append(json.RawMessage(nil), raw...)
		return nil
	})
	return out, err
}
