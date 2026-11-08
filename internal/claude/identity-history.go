package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/events"
)

// IdentityData is the native identity event payload using the resolved account label.
type IdentityData struct {
	Account          *string            `json:"account"`
	MetricAccount    *string            `json:"metric_account"`
	IntervalID       string             `json:"identity_interval_id"`
	EffectiveFrom    IdentityTimestamp  `json:"effective_from"`
	LastConfirmedAt  *IdentityTimestamp `json:"last_confirmed_at,omitempty"`
	EffectiveUntil   *IdentityTimestamp `json:"effective_until"`
	CredentialMtime  IdentityTimestamp  `json:"credential_mtime"`
	AccessExpiresAt  IdentityTimestamp  `json:"access_expires_at"`
	LoginExpiresAt   IdentityTimestamp  `json:"login_expires_at"`
	SubscriptionType *string            `json:"subscription_type"`
	RateTier         *string            `json:"rate_tier"`
	Metadata         IdentityMetadata   `json:"metadata"`
}

type identitySample struct {
	data        IdentityData
	at, last    time.Time
	fingerprint string
	raw         json.RawMessage
}

const maxIdentityHistory = 128

// IdentityBinding reports sampled evidence, not an exact historical login time.
type IdentityBinding struct {
	Account       *string
	MetricAccount *string
	IntervalID    string
	Quality       string // sampled, ambiguous, or unattributed
}

// IdentityAt resolves only this host/profile. A differing next sample makes the
// unobserved gap after the last confirmation ambiguous, never the new account.
func (p *Profile) IdentityAt(at time.Time) IdentityBinding {
	samples := p.identityHistory()
	return identityAt(samples, at)
}

func identityAt(samples []identitySample, at time.Time) IdentityBinding {
	for i := len(samples) - 1; i >= 0; i-- {
		s := samples[i]
		if at.Before(s.at) {
			continue
		}
		q := "sampled"
		if s.data.Account == nil {
			q = "unattributed"
		}
		if i+1 < len(samples) && at.After(s.last) && !sameAccount(s.data.Account, samples[i+1].data.Account) {
			q = "ambiguous"
		}
		var account *string
		if q == "sampled" {
			a := *s.data.Account
			account = &a
		}
		return IdentityBinding{Account: account, MetricAccount: s.data.MetricAccount, IntervalID: s.data.IntervalID, Quality: q}
	}
	return IdentityBinding{Quality: "unattributed"}
}

func (p *Profile) identityHistory() []identitySample {
	if p.Events == nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		return append([]identitySample(nil), p.history...)
	}
	p.historyMu.Lock()
	defer p.historyMu.Unlock()
	if !p.historyLoaded {
		saved, err := p.loadIdentityHistory()
		if err == nil {
			p.mu.Lock()
			for _, sample := range saved {
				p.mergeIdentity(sample)
			}
			p.mu.Unlock()
			p.historyLoaded = true
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, sample := range p.liveHistory {
		p.mergeIdentity(sample)
	}
	for _, sample := range p.history {
		p.mergeIdentity(sample)
	}
	return p.savedHistory
}

// Published slices are immutable; allocate only when evidence changes, not per lookup.
// Caller holds p.mu. Durable samples remain available beyond the live retention cap.
func (p *Profile) mergeIdentity(sample identitySample) {
	i := sort.Search(len(p.savedHistory), func(i int) bool { return !p.savedHistory[i].at.Before(sample.at) })
	if i < len(p.savedHistory) && p.savedHistory[i].data.IntervalID == sample.data.IntervalID {
		if !sample.last.After(p.savedHistory[i].last) {
			return
		}
		next := append([]identitySample(nil), p.savedHistory...)
		next[i] = sample
		p.savedHistory = next
		return
	}
	next := make([]identitySample, len(p.savedHistory)+1)
	copy(next, p.savedHistory[:i])
	next[i] = sample
	copy(next[i+1:], p.savedHistory[i:])
	p.savedHistory = next
}

func (p *Profile) loadIdentityHistory() ([]identitySample, error) {
	store := p.Events
	var saved []identitySample
	if store != nil {
		err := store.ReplaySource("claude-account", p.Alias, func(raw json.RawMessage) error {
			var row struct {
				Kind    string       `json:"kind"`
				Source  string       `json:"source"`
				Host    string       `json:"host"`
				Profile string       `json:"profile"`
				Harness string       `json:"harness"`
				Data    IdentityData `json:"data"`
			}
			if json.Unmarshal(raw, &row) != nil || row.Kind != "identity" || row.Source != "claude-account" || row.Host != p.Host || row.Profile != p.Alias || row.Harness != Harness || row.Data.EffectiveFrom.EpochSeconds == nil {
				return nil
			}
			when := time.Unix(0, int64(*row.Data.EffectiveFrom.EpochSeconds*1e9))
			last := when
			if row.Data.LastConfirmedAt != nil && row.Data.LastConfirmedAt.EpochSeconds != nil {
				last = time.Unix(0, int64(*row.Data.LastConfirmedAt.EpochSeconds*1e9))
			}
			saved = append(saved, identitySample{data: row.Data, at: when, last: last})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return saved, nil
}

func sameAccount(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// SampleIdentity retains one live binding. Durable identity events supply older bindings;
// a single pending envelope is retried verbatim before another observation is accepted.
func (p *Profile) SampleIdentity(now time.Time) error {
	facts, mtime, readErr := readIdentityFacts(p)
	d := IdentityData{CredentialMtime: mtime, AccessExpiresAt: expiryTime(facts.OAuth.ExpiresAt), LoginExpiresAt: expiryTime(facts.OAuth.RefreshTokenExpiresAt), SubscriptionType: facts.OAuth.SubscriptionType, RateTier: facts.OAuth.RateLimitTier, Metadata: facts.Account.IdentityMetadata}
	if err := p.ResolveAccount(); err == nil {
		if label, uuid := p.Account(); p.AccountAlias != "" || uuid != "" || (p.EmailLabel && label != "unknown") {
			d.Account, d.MetricAccount = &label, &label
		}
	}
	encoded, _ := json.Marshal(struct {
		Data       IdentityData
		Unreadable bool
	}{d, readErr != nil})
	sum := sha256.Sum256(encoded)
	fingerprint := hex.EncodeToString(sum[:])
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Events != nil && p.pending != nil {
		if err := p.Events.Append(events.Batch{Envelopes: []json.RawMessage{p.pending}}); err != nil {
			return err
		}
		p.pending = nil
	}
	if len(p.history) == 0 || p.history[len(p.history)-1].fingerprint != fingerprint {
		clock := identityTime(now, "collection")
		idSum := sha256.Sum256([]byte("identity-v1\x00" + p.Host + "\x00" + p.Alias + "\x00" + Harness + "\x00claude-account\x00" + now.UTC().Format(time.RFC3339Nano) + "\x00" + fingerprint))
		id := hex.EncodeToString(idSum[:])
		d.IntervalID, d.EffectiveFrom = id, clock
		d.LastConfirmedAt = &clock
		status := "observed"
		reasons, missing := []string{}, []string{}
		if d.Account == nil {
			status = "unknown"
			missing = append(missing, "account")
		}
		if readErr != nil {
			reasons = append(reasons, "credential_unreadable")
		}
		raw, err := json.Marshal(map[string]any{
			"schema_version": 1, "kind": "identity", "event_id": id, "source": "claude-account", "parser_version": "1",
			"source_schema_version": nil, "host": p.Host, "profile": p.Alias, "harness": Harness, "provider": Provider,
			"observed_at": clock, "collected_at": clock, "locator": map[string]any{"type": "probe", "probe_id": id, "part": "identity"},
			"quality": map[string]any{"status": status, "reasons": reasons, "missing_fields": missing}, "data": d,
		})
		if err != nil {
			return err
		}
		if p.Events != nil {
			for _, sample := range p.history {
				p.mergeIdentity(sample)
			}
			p.liveHistory = append(p.liveHistory, p.history...)
			if len(p.liveHistory) >= maxIdentityHistory {
				p.liveHistory = append([]identitySample(nil), p.liveHistory[len(p.liveHistory)-maxIdentityHistory+1:]...)
			}
			p.history = []identitySample{{d, now, now, fingerprint, raw}}
		} else {
			p.history = append(p.history, identitySample{d, now, now, fingerprint, nil})
			if len(p.history) > maxIdentityHistory {
				p.history = append([]identitySample(nil), p.history[len(p.history)-maxIdentityHistory:]...)
			}
		}
	} else {
		latest := &p.history[len(p.history)-1]
		latest.last = now
		clock := identityTime(now, "collection")
		latest.data.LastConfirmedAt = &clock
		if p.Events != nil {
			// Confirmation envelopes are append-only observations of the same interval.
			id := sha256.Sum256([]byte(latest.data.IntervalID + "\x00confirmation\x00" + now.UTC().Format(time.RFC3339Nano)))
			probe := hex.EncodeToString(id[:])
			latest.raw, _ = json.Marshal(map[string]any{
				"schema_version": 1, "kind": "identity", "event_id": probe, "source": "claude-account", "parser_version": "1",
				"source_schema_version": nil, "host": p.Host, "profile": p.Alias, "harness": Harness, "provider": Provider,
				"observed_at": clock, "collected_at": clock, "locator": map[string]any{"type": "probe", "probe_id": probe, "part": "identity"},
				"quality": map[string]any{"status": "observed", "reasons": []string{}, "missing_fields": []string{}}, "data": latest.data,
			})
		}
	}
	if p.Events != nil {
		latest := &p.history[len(p.history)-1]
		if latest.raw != nil {
			p.pending = latest.raw
		}
		if p.pending != nil {
			if err := p.Events.Append(events.Batch{Envelopes: []json.RawMessage{p.pending}}); err != nil {
				return err
			}
			p.pending = nil
		}
		latest.raw = nil
	}
	return nil
}
