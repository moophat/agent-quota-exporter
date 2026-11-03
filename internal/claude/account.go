package claude

import (
	"context"
	"encoding/json"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// AccountSource is `claude-account`: login state, credential expiry and account facts for one profile.
type AccountSource struct {
	P        *Profile
	Interval time.Duration
}

func (s *AccountSource) Name() string               { return "claude-account" }
func (s *AccountSource) Harness() string            { return Harness }
func (s *AccountSource) Profile() string            { return s.P.Alias }
func (s *AccountSource) MinInterval() time.Duration { return s.Interval }
func (s *AccountSource) Network() bool              { return false }

func (s *AccountSource) Collect(_ context.Context, now time.Time, _ json.RawMessage) (core.Result, json.RawMessage, error) {
	if err := realDir(s.P.Dir); err != nil {
		return core.Result{}, nil, err
	}
	if err := s.P.ResolveAccount(); err != nil {
		return core.Result{}, nil, err
	}
	label, _ := s.P.Account()
	has, exp, err := readCredentialFacts(s.P.Dir)
	if err != nil {
		return core.Result{}, nil, err
	}
	pl := core.Label{Name: "provider", Value: Provider}
	loggedIn := 0.0
	if has && exp.After(now) {
		loggedIn = 1
	}
	samples := []core.Sample{
		{Name: "agent_account_logged_in", Labels: s.P.labels(label, pl), Value: loggedIn},
		// plan: the subscription type is not on the sources.md allow-list yet, so it is reported as unknown.
		{Name: "agent_account_info", Labels: s.P.labels(label, pl, core.Label{Name: "plan", Value: "unknown"}), Value: 1},
	}
	if !exp.IsZero() {
		samples = append(samples, core.Sample{Name: "agent_credential_expiry_timestamp_seconds",
			Labels: s.P.labels(label, pl), Value: float64(exp.Unix())})
	}
	return core.Result{Samples: samples}, nil, nil
}
