package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/events"
)

func TestIdentityStoreFailurePreservesMetricsAndRetries(t *testing.T) {
	p := identityProfile(t, "profile-a")
	fail := true
	root := t.TempDir()
	s, err := events.Open(events.Options{Root: root, Budget: 1 << 20, Reserve: 1,
		Free: func(string) (int64, error) { return 1 << 30, nil },
		Sync: func(f *os.File) error {
			if fail {
				return errors.New("unavailable")
			}
			return f.Sync()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	p.Events = s
	result, _, err := collect(t, &AccountSource{P: p}, t0, nil)
	if err != nil || len(result.Samples) == 0 || result.Errors["other"] != 1 {
		t.Fatalf("metrics lost: %+v %v", result, err)
	}
	id := p.IdentityAt(t0).IntervalID
	fail = false
	if err := p.SampleIdentity(t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(root, "events", "month=*", "*.jsonl"))
	if len(paths) != 1 {
		t.Fatal(paths)
	}
	b, err := os.ReadFile(paths[0])
	if err != nil || bytes.Count(b, []byte{'\n'}) != 1 || !bytes.Contains(b, []byte(id)) {
		t.Fatalf("retry lost identity: %s %v", b, err)
	}
}

const identityFixture = `{"claudeAiOauth":{"expiresAt":1767232800000,"refreshTokenExpiresAt":1767319200000,"subscriptionType":"max","rateLimitTier":"tier-a"},"oauthAccount":{"emailAddress":"acct-a@example.com","organizationName":"Example","organizationType":"team","billingType":"subscription","seatTier":"standard","hasExtraUsageEnabled":false,"organizationRateLimitTier":"org-a","userRateLimitTier":"user-a"}}`

func identityProfile(t *testing.T, alias string) *Profile {
	t.Helper()
	p := &Profile{Host: "node-a", Alias: alias, Dir: t.TempDir(), IdentityKey: bytes.Repeat([]byte{1}, 32)}
	write(t, filepath.Join(p.Dir, ".credentials.json"), []byte(identityFixture))
	return p
}

func TestIdentityAccountSwitchHistory(t *testing.T) {
	p := identityProfile(t, "profile-a")
	if err := p.SampleIdentity(t0); err != nil {
		t.Fatal(err)
	}
	a := p.IdentityAt(t0)
	if a.Account == nil || !strings.HasPrefix(*a.Account, "acct_") || len(*a.Account) != 69 {
		t.Fatalf("binding %+v", a)
	}
	if err := p.SampleIdentity(t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Dir, ".credentials.json"), []byte(strings.ReplaceAll(identityFixture, "acct-a@example.com", "acct-b@example.com")))
	if err := p.SampleIdentity(t0.Add(3 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := p.IdentityAt(t0); got.Account == nil || *got.Account != *a.Account {
		t.Fatal("old binding lost")
	}
	if got := p.IdentityAt(t0.Add(2 * time.Minute)); got.Quality != "ambiguous" || got.Account != nil {
		t.Fatalf("gap %+v", got)
	}
	if got := p.IdentityAt(t0.Add(3 * time.Minute)); got.Account == nil || *got.Account == *a.Account {
		t.Fatal("new binding missing")
	}
	if got := p.IdentityAt(t0.Add(-time.Second)); got.Quality != "unattributed" {
		t.Fatal(got)
	}
	q := identityProfile(t, "profile-b")
	if err := q.SampleIdentity(t0.Add(4 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := q.IdentityAt(t0.Add(4 * time.Minute)); got.Account == nil || *got.Account != *a.Account {
		t.Fatal("profiles contaminated")
	}
}

func TestIdentityUnknownUnreadable(t *testing.T) {
	p := identityProfile(t, "profile-a")
	for i, body := range []string{identityFixture, `{}`, `{invalid`} {
		write(t, filepath.Join(p.Dir, ".credentials.json"), []byte(body))
		if err := p.SampleIdentity(t0.Add(time.Duration(i) * time.Minute)); err != nil {
			t.Fatal(err)
		}
		if i > 0 && p.IdentityAt(t0.Add(time.Duration(i)*time.Minute)).Account != nil {
			t.Fatal("unknown attributed")
		}
	}
	os.Remove(filepath.Join(p.Dir, ".credentials.json"))
	if err := p.SampleIdentity(t0.Add(4 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if p.IdentityAt(t0.Add(4*time.Minute)).Account != nil {
		t.Fatal("missing credential attributed")
	}
}

func TestIdentityAllowlistAndDurableCollection(t *testing.T) {
	var fixture map[string]map[string]any
	if err := json.Unmarshal([]byte(identityFixture), &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture["claudeAiOauth"]) != 4 || len(fixture["oauthAccount"]) != 8 || strings.Contains(identityFixture, `"accessToken"`) || strings.Contains(identityFixture, `"refreshToken"`) {
		t.Fatal("fixture allowlist drift")
	}
	p := identityProfile(t, "profile-a")
	root := t.TempDir()
	s, err := events.Open(events.Options{Root: root, Budget: 1 << 20, Reserve: 1, Free: func(string) (int64, error) { return 1 << 30, nil }})
	if err != nil {
		t.Fatal(err)
	}
	p.Events = s
	if _, _, err := collect(t, &AccountSource{P: p}, t0, nil); err != nil {
		t.Fatal(err)
	}
	if label, _ := p.Account(); label != *p.IdentityAt(t0).Account {
		t.Fatal("metric and event account identities differ")
	}
	if err := p.SampleIdentity(t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(root, "events", "month=*", "*.jsonl"))
	if len(paths) != 1 {
		t.Fatal(paths)
	}
	b, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(b, []byte{'\n'}) != 1 || bytes.Contains(b, []byte("example.com")) {
		t.Fatalf("unsafe or repeated event %s", b)
	}
	var row struct {
		Data IdentityData `json:"data"`
	}
	if err := json.Unmarshal(b, &row); err != nil {
		t.Fatal(err)
	}
	d := row.Data
	if d.Account == nil || d.SubscriptionType == nil || *d.SubscriptionType != "max" || d.RateTier == nil || *d.RateTier != "tier-a" || d.AccessExpiresAt.EpochSeconds == nil || *d.AccessExpiresAt.EpochSeconds != 1767232800 || d.LoginExpiresAt.EpochSeconds == nil || *d.LoginExpiresAt.EpochSeconds != 1767319200 || d.CredentialMtime.EpochSeconds == nil {
		t.Fatalf("lost facts %+v", d)
	}
	m := d.Metadata
	if m.OrganizationName == nil || *m.OrganizationName != "Example" || m.OrganizationType == nil || *m.OrganizationType != "team" || m.BillingType == nil || *m.BillingType != "subscription" || m.SeatTier == nil || *m.SeatTier != "standard" || m.HasExtraUsageEnabled == nil || *m.HasExtraUsageEnabled || m.OrganizationRateLimitTier == nil || *m.OrganizationRateLimitTier != "org-a" || m.UserRateLimitTier == nil || *m.UserRateLimitTier != "user-a" {
		t.Fatalf("lost metadata %+v", m)
	}
}

func TestIdentityOptionalFallbackKeepsAliasAndMetadata(t *testing.T) {
	p := identityProfile(t, "profile-a")
	p.AccountAlias = "operator-alias"
	credential := `{"claudeAiOauth":{"expiresAt":1767232800000},"oauthAccount":{"organizationName":"Kept"}}`
	write(t, filepath.Join(p.Dir, ".credentials.json"), []byte(credential))
	if err := p.SampleIdentity(t0); err != nil {
		t.Fatal(err)
	}
	got := p.IdentityAt(t0)
	if got.Account == nil || *got.Account != "operator-alias" {
		t.Fatalf("alias lost: %+v", got)
	}
	if p.history[0].data.Metadata.OrganizationName == nil || *p.history[0].data.Metadata.OrganizationName != "Kept" {
		t.Fatalf("credential metadata lost: %+v", p.history[0].data.Metadata)
	}
	p = identityProfile(t, "profile-b")
	write(t, filepath.Join(p.Dir, ".credentials.json"), []byte(`{"claudeAiOauth":{"expiresAt":1767232800000},"oauthAccount":{"emailAddress":"credential@example.com"}}`))
	write(t, filepath.Join(p.Dir, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"fallback@example.com","organizationName":"Kept"}}`))
	if err := p.SampleIdentity(t0); err != nil {
		t.Fatal(err)
	}
	if p.history[0].data.Account == nil || p.history[0].data.Metadata.OrganizationName == nil || *p.history[0].data.Metadata.OrganizationName != "Kept" {
		t.Fatalf("fallback overwrote credential metadata: %+v", p.history[0].data)
	}
}

func TestIdentityHistoryRebuildsFromStoreAndStaysBounded(t *testing.T) {
	p := identityProfile(t, "profile-a")
	root := t.TempDir()
	s, err := events.Open(events.Options{Root: root, Budget: 1 << 20, Reserve: 1, Free: func(string) (int64, error) { return 1 << 30, nil }})
	if err != nil {
		t.Fatal(err)
	}
	p.Events = s
	if err := p.SampleIdentity(t0); err != nil {
		t.Fatal(err)
	}
	first := p.IdentityAt(t0)
	write(t, filepath.Join(p.Dir, ".credentials.json"), []byte(strings.ReplaceAll(identityFixture, "acct-a@example.com", "acct-b@example.com")))
	if err := p.SampleIdentity(t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(p.history) != 1 || p.pending != nil {
		t.Fatalf("unbounded durable history: %d pending %v", len(p.history), p.pending != nil)
	}
	restarted := identityProfile(t, "profile-a")
	restarted.Events = s
	if got := restarted.IdentityAt(t0); got.Account == nil || first.Account == nil || *got.Account != *first.Account {
		t.Fatalf("restart lost old binding: %+v", got)
	}
	plain := identityProfile(t, "plain")
	for i := 0; i < maxIdentityHistory+1; i++ {
		write(t, filepath.Join(plain.Dir, ".credentials.json"), []byte(strings.ReplaceAll(identityFixture, "acct-a@example.com", string(rune('b'+i))+"@example.com")))
		if err := plain.SampleIdentity(t0.Add(time.Duration(i) * time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if len(plain.history) != maxIdentityHistory {
		t.Fatalf("history grew without bound: %d", len(plain.history))
	}
	for _, sample := range plain.history {
		if sample.raw != nil {
			t.Fatal("event-disabled history retained serialized envelope")
		}
	}
}
