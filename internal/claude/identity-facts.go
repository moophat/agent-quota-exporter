package claude

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// IdentityTimestamp preserves the source value separately from its normalized clock.
type IdentityTimestamp struct {
	Original     any      `json:"original"`
	EpochSeconds *float64 `json:"epoch_seconds"`
	Quality      string   `json:"quality"`
}

func identityTime(t time.Time, quality string) IdentityTimestamp {
	n := float64(t.UnixNano()) / 1e9
	return IdentityTimestamp{t.UTC().Format(time.RFC3339Nano), &n, quality}
}

func expiryTime(n *int64) IdentityTimestamp {
	if n == nil {
		return IdentityTimestamp{Quality: "unknown"}
	}
	e := float64(*n) / 1000
	return IdentityTimestamp{*n, &e, "source"}
}

// IdentityMetadata is the complete closed allowlist; absent values serialize as null.
type IdentityMetadata struct {
	OrganizationName          *string `json:"organizationName"`
	OrganizationType          *string `json:"organizationType"`
	BillingType               *string `json:"billingType"`
	SeatTier                  *string `json:"seatTier"`
	HasExtraUsageEnabled      *bool   `json:"hasExtraUsageEnabled"`
	OrganizationRateLimitTier *string `json:"organizationRateLimitTier"`
	UserRateLimitTier         *string `json:"userRateLimitTier"`
}

type identityFacts struct {
	OAuth struct {
		ExpiresAt             *int64  `json:"expiresAt"`
		RefreshTokenExpiresAt *int64  `json:"refreshTokenExpiresAt"`
		SubscriptionType      *string `json:"subscriptionType"`
		RateLimitTier         *string `json:"rateLimitTier"`
	} `json:"claudeAiOauth"`
	Account identityAccount `json:"oauthAccount"`
}

type identityAccount struct {
	Email *string `json:"emailAddress"`
	UUID  *string `json:"accountUuid"`
	IdentityMetadata
}

// readIdentityFacts decodes only named metadata leaves, never token values or arbitrary objects.
func readIdentityFacts(p *Profile) (identityFacts, IdentityTimestamp, error) {
	var facts identityFacts
	unknown := IdentityTimestamp{Quality: "unknown"}
	if err := realDir(p.Dir); err != nil {
		return facts, unknown, err
	}
	f, fi, err := openRegular(filepath.Join(p.Dir, ".credentials.json"))
	if err != nil {
		return facts, unknown, err
	}
	defer f.Close()
	mtime := identityTime(fi.ModTime(), "source")
	if fi.Size() > 1<<20 {
		return facts, mtime, core.Fail(core.ReasonParse)
	}
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return facts, mtime, err
	}
	defer wipe(b)
	if len(b) > 1<<20 {
		return facts, mtime, core.Fail(core.ReasonParse)
	}
	if json.Unmarshal(b, &facts) != nil {
		return identityFacts{}, mtime, core.Fail(core.ReasonParse)
	}
	// Claude normally keeps oauthAccount in .claude.json, not credentials.
	// Always compare identities, even with complete credentials, before filling missing leaves.
	{
		b, err = readSmall(ClaudeJSONPath(p.Dir, p.ConfigDir), 64<<20)
		if err == nil {
			defer wipe(b)
			var account struct {
				Account identityAccount `json:"oauthAccount"`
			}
			if json.Unmarshal(b, &account) != nil {
				return identityFacts{}, mtime, core.Fail(core.ReasonParse)
			}
			if identityAccountsConflict(facts.Account, account.Account) {
				return identityFacts{}, mtime, core.Fail(core.ReasonParse)
			}
			mergeIdentityAccount(&facts.Account, account.Account)
		} else if !isNotFound(err) {
			return facts, mtime, err
		}
	}
	return facts, mtime, nil
}

func identityAccountsConflict(a, b identityAccount) bool {
	return a.Email != nil && b.Email != nil && *a.Email != "" && *b.Email != "" && *a.Email != *b.Email ||
		a.UUID != nil && b.UUID != nil && *a.UUID != "" && *b.UUID != "" && *a.UUID != *b.UUID
}

func isNotFound(err error) bool {
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	var sourceErr *core.SourceError
	return errors.As(err, &sourceErr) && sourceErr.Reason == core.ReasonNotFound
}

func mergeIdentityAccount(dst *identityAccount, fallback identityAccount) {
	if dst.Email == nil {
		dst.Email = fallback.Email
	}
	if dst.OrganizationName == nil {
		dst.OrganizationName = fallback.OrganizationName
	}
	if dst.OrganizationType == nil {
		dst.OrganizationType = fallback.OrganizationType
	}
	if dst.BillingType == nil {
		dst.BillingType = fallback.BillingType
	}
	if dst.SeatTier == nil {
		dst.SeatTier = fallback.SeatTier
	}
	if dst.HasExtraUsageEnabled == nil {
		dst.HasExtraUsageEnabled = fallback.HasExtraUsageEnabled
	}
	if dst.OrganizationRateLimitTier == nil {
		dst.OrganizationRateLimitTier = fallback.OrganizationRateLimitTier
	}
	if dst.UserRateLimitTier == nil {
		dst.UserRateLimitTier = fallback.UserRateLimitTier
	}
}
