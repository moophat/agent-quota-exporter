package claude

// AI-Model: t9r/cx/gpt-6.1-sol
import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/moophat/agent-quota-exporter/internal/events"
)

func TestPlainEmailIdentityReplay(t *testing.T) {
	p := identityProfile(t, "profile-a")
	p.EmailLabel = true
	write(t, filepath.Join(p.Dir, ".credentials.json"), []byte(identityFixture))
	write(t, filepath.Join(p.Dir, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"acct-a@example.com","accountUuid":"owner-a"}}`))
	store, err := events.Open(events.Options{Root: t.TempDir(), Budget: 1 << 20, Reserve: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p.Events = store
	if err := p.SampleIdentity(t0); err != nil {
		t.Fatal(err)
	}
	fresh := &Profile{Host: p.Host, Alias: p.Alias, Events: store}
	b := fresh.IdentityAt(t0)
	if b.Account == nil || b.MetricAccount == nil || *b.Account != "acct-a@example.com" || *b.MetricAccount != *b.Account {
		t.Fatalf("replayed binding: %+v", b)
	}
	if err := store.Replay(func(raw json.RawMessage) error {
		var row struct{ Data IdentityData }
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		if row.Data.Account == nil || *row.Data.Account != "acct-a@example.com" {
			t.Errorf("stored identity: %+v", row.Data)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
