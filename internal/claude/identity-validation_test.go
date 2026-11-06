package claude

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moophat/agent-quota-exporter/internal/events"
)

func TestIdentityOrganizationNameValidation(t *testing.T) {
	for _, name := range []string{"Example @ Workspace", "Example bearer private-value"} {
		t.Run(name, func(t *testing.T) {
			p := identityProfile(t, "profile-a")
			// Token-free logged-in fixture: integer expiries, string plan/tier
			// metadata, and a boolean extra-usage flag, including fallback metadata.
			var fixture map[string]any
			if err := json.Unmarshal([]byte(identityFixture), &fixture); err != nil {
				t.Fatal(err)
			}
			account := fixture["oauthAccount"].(map[string]any)
			account["organizationName"] = name
			delete(fixture, "oauthAccount")
			credentials, _ := json.Marshal(fixture)
			metadata, _ := json.Marshal(map[string]any{"oauthAccount": account})
			write(t, filepath.Join(p.Dir, ".credentials.json"), credentials)
			write(t, filepath.Join(p.Dir, ".claude.json"), metadata)
			store, err := events.Open(events.Options{Root: t.TempDir(), Budget: 1 << 20, Reserve: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			p.Events = store
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			err = p.SampleIdentity(t0)
			if strings.Contains(name, "bearer") {
				if err == nil || !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "field=data.metadata.organizationName") || strings.Contains(logs.String(), name) || strings.Contains(logs.String(), "private-value") {
					t.Fatalf("unsafe or missing rejection: err=%v logs=%s", err, logs.String())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			if err := store.Replay(func(raw json.RawMessage) error {
				var row struct {
					Data IdentityData `json:"data"`
				}
				if err := json.Unmarshal(raw, &row); err != nil {
					return err
				}
				if row.Data.Metadata.OrganizationName == nil || *row.Data.Metadata.OrganizationName != name {
					t.Fatal("organization metadata lost")
				}
				count++
				return nil
			}); err != nil || count != 1 {
				t.Fatalf("replay count=%d err=%v", count, err)
			}
		})
	}
}
