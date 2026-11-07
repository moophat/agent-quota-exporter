package claude

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/events"
)

// IdentityData is the native identity event payload. Raw emails never leave the reader.
type IdentityData struct {
	Account          *string            `json:"account"`
	MetricAccount    *string            `json:"metric_account"`
	IntervalID       string             `json:"identity_interval_id"`
	EffectiveFrom    IdentityTimestamp  `json:"effective_from"`
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

// Match email-shaped tokens without consuming possessive suffixes in display names.
var organizationEmail = regexp.MustCompile(`[A-Za-z0-9.!#$%&*+/=?^_` + "`" + `{|}~-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+`)

func (p *Profile) opaqueEmail(email string) string {
	mac := hmac.New(sha256.New, p.IdentityKey)
	mac.Write([]byte("account-v1\x00" + Provider + "\x00" + strings.TrimSpace(email)))
	return "acct_" + hex.EncodeToString(mac.Sum(nil))
}

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
	p.historyOnce.Do(func() {
		p.savedHistory = p.loadIdentityHistory()
	})
	p.mu.Lock()
	live := append(append([]identitySample(nil), p.liveHistory...), p.history...)
	p.mu.Unlock()
	samples := append(append([]identitySample(nil), p.savedHistory...), live...)
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].at.Before(samples[j].at) })
	return samples
}

func (p *Profile) loadIdentityHistory() []identitySample {
	store := p.Events
	var saved []identitySample
	if store != nil {
		_ = store.ReplaySource("claude-account", p.Alias, func(raw json.RawMessage) error {
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
			saved = append(saved, identitySample{data: row.Data, at: when, last: when})
			return nil
		})
	}
	return saved
}

func sameAccount(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// SampleIdentity retains one live binding. Durable identity events supply older bindings;
// a single pending envelope is retried verbatim before another observation is accepted.
func (p *Profile) SampleIdentity(now time.Time) error {
	facts, mtime, readErr := readIdentityFacts(p)
	if facts.Account.OrganizationName != nil {
		redacted := organizationEmail.ReplaceAllStringFunc(*facts.Account.OrganizationName, func(email string) string {
			if len(p.IdentityKey) < 32 {
				return "<email>"
			}
			return p.opaqueEmail(email)
		})
		facts.Account.OrganizationName = &redacted
	}
	d := IdentityData{CredentialMtime: mtime, AccessExpiresAt: expiryTime(facts.OAuth.ExpiresAt), LoginExpiresAt: expiryTime(facts.OAuth.RefreshTokenExpiresAt), SubscriptionType: facts.OAuth.SubscriptionType, RateTier: facts.OAuth.RateLimitTier, Metadata: facts.Account.IdentityMetadata}
	// Resolve independently of private history so key provisioning/rotation cannot
	// rename metric series. Email compatibility labels are never retained in events.
	// Only a real label joins: an operator alias, or the hash of a known account UUID.
	if err := p.ResolveAccount(); err == nil && !p.EmailLabel {
		if label, uuid := p.Account(); p.AccountAlias != "" || uuid != "" {
			d.MetricAccount = &label
		}
	}
	if readErr == nil {
		account := p.AccountAlias
		if account == "" && facts.Account.Email != nil && strings.TrimSpace(*facts.Account.Email) != "" && (p.Events != nil || len(p.IdentityKey) >= 32) {
			if len(p.IdentityKey) < 32 {
				return errors.New("identity: missing installation key")
			}
			account = p.opaqueEmail(*facts.Account.Email)
		}
		if account != "" {
			d.Account = &account
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
			p.liveHistory = append(p.liveHistory, p.history...)
			p.history = []identitySample{{d, now, now, fingerprint, raw}}
		} else {
			p.history = append(p.history, identitySample{d, now, now, fingerprint, nil})
			if len(p.history) > maxIdentityHistory {
				p.history = append([]identitySample(nil), p.history[len(p.history)-maxIdentityHistory:]...)
			}
		}
	} else {
		p.history[len(p.history)-1].last = now
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
