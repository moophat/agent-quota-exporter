package claude

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/events"
)

// IdentityData is the native identity event payload. Raw emails never leave the reader.
type IdentityData struct {
	Account          *string            `json:"account"`
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

// IdentityBinding reports sampled evidence, not an exact historical login time.
type IdentityBinding struct {
	Account    *string
	IntervalID string
	Quality    string // sampled, ambiguous, or unattributed
}

// IdentityAt resolves only this host/profile. A differing next sample makes the
// unobserved gap after the last confirmation ambiguous, never the new account.
func (p *Profile) IdentityAt(at time.Time) IdentityBinding {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.history) - 1; i >= 0; i-- {
		s := p.history[i]
		if at.Before(s.at) {
			continue
		}
		q := "sampled"
		if s.data.Account == nil {
			q = "unattributed"
		}
		if i+1 < len(p.history) && at.After(s.last) && !sameAccount(s.data.Account, p.history[i+1].data.Account) {
			q = "ambiguous"
		}
		var account *string
		if q == "sampled" {
			a := *s.data.Account
			account = &a
		}
		return IdentityBinding{account, s.data.IntervalID, q}
	}
	return IdentityBinding{Quality: "unattributed"}
}

func sameAccount(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// SampleIdentity retains history even without an event sink. Pending envelopes are
// retried verbatim after a failed append, preserving event IDs and collection clocks.
func (p *Profile) SampleIdentity(now time.Time) error {
	facts, mtime, readErr := readIdentityFacts(p)
	d := IdentityData{CredentialMtime: mtime, AccessExpiresAt: expiryTime(facts.OAuth.ExpiresAt), LoginExpiresAt: expiryTime(facts.OAuth.RefreshTokenExpiresAt), SubscriptionType: facts.OAuth.SubscriptionType, RateTier: facts.OAuth.RateLimitTier, Metadata: facts.Account.IdentityMetadata}
	if readErr == nil {
		account := p.AccountAlias
		if account == "" && facts.Account.Email != nil && strings.TrimSpace(*facts.Account.Email) != "" && (p.Events != nil || len(p.IdentityKey) >= 32) {
			if len(p.IdentityKey) < 32 {
				return errors.New("identity: missing installation key")
			}
			mac := hmac.New(sha256.New, p.IdentityKey)
			mac.Write([]byte("account-v1\x00" + Provider + "\x00" + strings.TrimSpace(*facts.Account.Email)))
			account = "acct_" + hex.EncodeToString(mac.Sum(nil))
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
		p.history = append(p.history, identitySample{d, now, now, fingerprint, raw})
	} else {
		p.history[len(p.history)-1].last = now
	}
	if p.Events != nil {
		var pending []json.RawMessage
		for _, s := range p.history {
			if s.raw != nil {
				pending = append(pending, s.raw)
			}
		}
		if err := p.Events.Append(events.Batch{Envelopes: pending}); err != nil {
			return err
		}
		for i := range p.history {
			p.history[i].raw = nil
		}
	}
	return nil
}
