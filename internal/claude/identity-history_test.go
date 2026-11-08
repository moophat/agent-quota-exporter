package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	if err != nil || bytes.Count(b, []byte{'\n'}) != 2 || !bytes.Contains(b, []byte(id)) {
		t.Fatalf("retry lost identity: %s %v", b, err)
	}
}

const identityFixture = `{"claudeAiOauth":{"expiresAt":1767232800000,"refreshTokenExpiresAt":1767319200000,"subscriptionType":"max","rateLimitTier":"tier-a"},"oauthAccount":{"emailAddress":"acct-a@example.com","organizationName":"Example","organizationType":"team","billingType":"subscription","seatTier":"standard","hasExtraUsageEnabled":false,"organizationRateLimitTier":"org-a","userRateLimitTier":"user-a"}}`

func identityProfile(t *testing.T, alias string) *Profile {
	t.Helper()
	p := &Profile{Host: "node-a", Alias: alias, Dir: t.TempDir(), EmailLabel: true}
	write(t, filepath.Join(p.Dir, ".credentials.json"), []byte(identityFixture))
	write(t, filepath.Join(p.Dir, ".claude.json"), []byte(identityFixture))
	p.jsonMod = time.Time{}
	return p
}

func TestIdentityHistoryRetriesFailedLoad(t *testing.T) {
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
	paths, _ := filepath.Glob(filepath.Join(root, "events", "month=*", "*.jsonl"))
	if len(paths) != 1 {
		t.Fatal(paths)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths[0], []byte("invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted := identityProfile(t, "profile-a")
	restarted.Events = s
	if got := restarted.IdentityAt(t0); got.Account != nil {
		t.Fatal("failed load published history")
	}
	if err := os.WriteFile(paths[0], data, 0600); err != nil {
		t.Fatal(err)
	}
	if got := restarted.IdentityAt(t0); got.Account == nil {
		t.Fatal("readable history was not retried")
	}
}

func TestIdentityConfirmationSurvivesRestart(t *testing.T) {
	p := identityProfile(t, "profile-a")
	s, err := events.Open(events.Options{Root: t.TempDir(), Budget: 1 << 22, Reserve: 1, Free: func(string) (int64, error) { return 1 << 30, nil }})
	if err != nil {
		t.Fatal(err)
	}
	p.Events = s
	for _, at := range []time.Time{t0, t0.Add(2 * time.Minute)} {
		if err := p.SampleIdentity(at); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(p.Dir, ".credentials.json"), []byte(strings.ReplaceAll(identityFixture, "acct-a@example.com", "acct-b@example.com")))
	write(t, filepath.Join(p.Dir, ".claude.json"), []byte(strings.ReplaceAll(identityFixture, "acct-a@example.com", "acct-b@example.com")))
	p.jsonMod = time.Time{}
	if err := p.SampleIdentity(t0.Add(3 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	before := p.IdentityAt(t0.Add(time.Minute))
	restarted := identityProfile(t, "profile-a")
	restarted.Events = s
	after := restarted.IdentityAt(t0.Add(time.Minute))
	if before.Quality != "sampled" || after.Quality != before.Quality || after.Account == nil || *after.Account != *before.Account {
		t.Fatalf("before=%+v after=%+v", before, after)
	}
}

func TestIdentityLiveRetentionKeepsOlderBindings(t *testing.T) {
	p := identityProfile(t, "profile-a")
	s, err := events.Open(events.Options{Root: t.TempDir(), Budget: 1 << 24, Reserve: 1, Free: func(string) (int64, error) { return 1 << 30, nil }})
	if err != nil {
		t.Fatal(err)
	}
	p.Events = s
	if err := p.SampleIdentity(t0); err != nil {
		t.Fatal(err)
	}
	first := p.IdentityAt(t0)
	for i := 1; i < maxIdentityHistory+5; i++ {
		p.AccountAlias = strings.Repeat("a", i)
		if err := p.SampleIdentity(t0.Add(time.Duration(i) * time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.liveHistory)+len(p.history) > maxIdentityHistory {
		t.Fatalf("retained %d live samples", len(p.liveHistory)+len(p.history))
	}
	if got := p.IdentityAt(t0); got.Account == nil || *got.Account != *first.Account {
		t.Fatal("older durable binding lost")
	}
}

func TestIdentityHistoryLoadsOnce(t *testing.T) {
	p := identityProfile(t, "profile-a")
	calls := 0
	s, err := events.Open(events.Options{Root: t.TempDir(), Budget: 1 << 20, Reserve: 1,
		Free: func(string) (int64, error) { return 1 << 30, nil }, Replay: func() { calls++ }})
	if err != nil {
		t.Fatal(err)
	}
	p.Events = s
	if err := p.SampleIdentity(t0); err != nil {
		t.Fatal(err)
	}
	first := p.IdentityAt(t0)
	if calls != 1 {
		t.Fatalf("first load: %d replays", calls)
	}
	write(t, filepath.Join(p.Dir, ".credentials.json"), []byte(strings.ReplaceAll(identityFixture, "acct-a@example.com", "acct-b@example.com")))
	write(t, filepath.Join(p.Dir, ".claude.json"), []byte(strings.ReplaceAll(identityFixture, "acct-a@example.com", "acct-b@example.com")))
	p.jsonMod = time.Time{}
	if err := p.SampleIdentity(t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := p.IdentityAt(t0); got.Account == nil || first.Account == nil || *got.Account != *first.Account {
		t.Fatal("old identity lost")
	}
	if got := p.IdentityAt(t0.Add(time.Minute)); got.Account == nil || *got.Account == *first.Account {
		t.Fatal("live switch lost")
	}
	if calls != 1 {
		t.Fatalf("later history lookup replayed: %d calls", calls)
	}
}

func TestIdentityAccountSwitchHistory(t *testing.T) {
	p := identityProfile(t, "profile-a")
	if err := p.SampleIdentity(t0); err != nil {
		t.Fatal(err)
	}
	a := p.IdentityAt(t0)
	if a.Account == nil || *a.Account != "acct-a@example.com" {
		t.Fatalf("binding %+v", a)
	}
	if err := p.SampleIdentity(t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.Dir, ".credentials.json"), []byte(strings.ReplaceAll(identityFixture, "acct-a@example.com", "acct-b@example.com")))
	write(t, filepath.Join(p.Dir, ".claude.json"), []byte(strings.ReplaceAll(identityFixture, "acct-a@example.com", "acct-b@example.com")))
	p.jsonMod = time.Time{}
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
		write(t, filepath.Join(p.Dir, ".claude.json"), []byte(body))
		p.jsonMod = time.Time{}
		if err := p.SampleIdentity(t0.Add(time.Duration(i) * time.Minute)); err != nil {
			t.Fatal(err)
		}
		if i > 0 && p.IdentityAt(t0.Add(time.Duration(i)*time.Minute)).Account != nil {
			t.Fatal("unknown attributed")
		}
	}
	os.Remove(filepath.Join(p.Dir, ".credentials.json"))
	os.Remove(filepath.Join(p.Dir, ".claude.json"))
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
	if bytes.Count(b, []byte{'\n'}) != 2 || !bytes.Contains(b, []byte("acct-a@example.com")) {
		t.Fatalf("unsafe or repeated event %s", b)
	}
	var row struct {
		Data IdentityData `json:"data"`
	}
	if err := json.Unmarshal(bytes.Split(b, []byte{'\n'})[0], &row); err != nil {
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

func TestMetricAccountStableWithEventStore(t *testing.T) {
	for _, alias := range []string{"", "operator-alias"} {
		t.Run("alias="+alias, func(t *testing.T) {
			p := identityProfile(t, "profile-a")
			p.AccountAlias = alias
			p.EmailLabel = false
			write(t, filepath.Join(p.Dir, ".claude.json"), []byte(`{"oauthAccount":{"accountUuid":"synthetic-account-a"}}`))
			before, _, err := collect(t, &AccountSource{P: p}, t0, nil)
			if err != nil {
				t.Fatal(err)
			}
			label, _ := p.Account()
			if alias == "" && !strings.HasPrefix(label, "acct-") {
				t.Fatalf("not a short hash: %s", label)
			}
			s, err := events.Open(events.Options{Root: t.TempDir(), Budget: 1 << 20, Reserve: 1, Free: func(string) (int64, error) { return 1 << 30, nil }})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			p.Events = s
			after, _, err := collect(t, &AccountSource{P: p}, t0.Add(time.Minute), nil)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(before.Samples)
			a, _ := json.Marshal(after.Samples)
			if !bytes.Equal(a, b) {
				t.Fatalf("metrics changed with identity key:\nbefore %s\nafter %s", b, a)
			}
			if err := s.Replay(func(raw json.RawMessage) error {
				var row struct {
					Data IdentityData `json:"data"`
				}
				if err := json.Unmarshal(raw, &row); err != nil {
					return err
				}
				if row.Data.MetricAccount == nil || *row.Data.MetricAccount != label {
					t.Fatalf("missing metric join: %+v", row.Data)
				}
				if row.Data.Account == nil || *row.Data.Account != label {
					t.Fatalf("private identity lost: %+v", row.Data)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMetricAccountNullWithoutAliasOrAccount(t *testing.T) {
	// Credentials readable, no .claude.json: no account UUID, so no join label.
	for alias, want := range map[string]*string{"": nil, "unknown": func() *string { s := "unknown"; return &s }()} {
		t.Run("alias="+alias, func(t *testing.T) {
			p := identityProfile(t, "profile-a")
			p.AccountAlias = alias
			os.Remove(filepath.Join(p.Dir, ".claude.json"))
			s, err := events.Open(events.Options{Root: t.TempDir(), Budget: 1 << 20, Reserve: 1, Free: func(string) (int64, error) { return 1 << 30, nil }})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			p.Events = s
			if err := p.SampleIdentity(t0); err != nil {
				t.Fatal(err)
			}
			rows := 0
			if err := s.Replay(func(raw json.RawMessage) error {
				var row struct {
					Data IdentityData `json:"data"`
				}
				if err := json.Unmarshal(raw, &row); err != nil {
					return err
				}
				rows++
				got := row.Data.MetricAccount
				if (want == nil) != (got == nil) || (want != nil && *got != *want) {
					t.Fatalf("metric_account %v, want %v", got, want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if rows == 0 {
				t.Fatal("no identity event written")
			}
		})
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
	write(t, filepath.Join(p.Dir, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"credential@example.com","organizationName":"Kept"}}`))
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
	write(t, filepath.Join(p.Dir, ".claude.json"), []byte(strings.ReplaceAll(identityFixture, "acct-a@example.com", "acct-b@example.com")))
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
		write(t, filepath.Join(plain.Dir, ".credentials.json"), []byte(strings.ReplaceAll(identityFixture, "acct-a@example.com", fmt.Sprintf("acct-%d@example.com", i))))
		write(t, filepath.Join(plain.Dir, ".claude.json"), []byte(strings.ReplaceAll(identityFixture, "acct-a@example.com", fmt.Sprintf("acct-%d@example.com", i))))
		plain.jsonMod = time.Time{}
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
