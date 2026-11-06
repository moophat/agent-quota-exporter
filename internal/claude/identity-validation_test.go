package claude

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moophat/agent-quota-exporter/internal/events"
)

func TestIdentityOrganizationNameValidation(t *testing.T) {
	for _, key := range []bool{true, false} {
		for _, name := range []string{"acct-a@example.com's Organization", "Example bearer private-value"} {
			t.Run(name+map[bool]string{true: "-key", false: "-no-key"}[key], func(t *testing.T) {
				p := identityProfile(t, "profile-a")
				want := p.opaqueEmail("acct-a@example.com") + "'s Organization"
				if !key {
					p.IdentityKey = nil
					p.AccountAlias = "account-a"
					want = "<email>'s Organization"
				}
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
				root := t.TempDir()
				store, err := events.Open(events.Options{Root: root, Budget: 1 << 20, Reserve: 1})
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
					if err == nil || !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "source=claude-account") || !strings.Contains(logs.String(), "profile=profile-a") || !strings.Contains(logs.String(), "field=data.metadata.organizationName") || strings.Contains(logs.String(), name) || strings.Contains(logs.String(), "private-value") {
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
					if row.Data.Metadata.OrganizationName == nil || *row.Data.Metadata.OrganizationName != want {
						t.Fatal("organization metadata not redacted with expected identity")
					}
					if key && (row.Data.Account == nil || want != *row.Data.Account+"'s Organization") {
						t.Fatal("organization email uses a different account identity")
					}
					count++
					return nil
				}); err != nil || count != 1 {
					t.Fatalf("replay count=%d err=%v", count, err)
				}
				paths, err := filepath.Glob(filepath.Join(root, "events", "month=*", "*.jsonl"))
				if err != nil || len(paths) != 1 {
					t.Fatalf("shards: %v %v", paths, err)
				}
				shard, err := os.ReadFile(paths[0])
				if err != nil || bytes.Contains(shard, []byte("@")) || bytes.Contains(shard, []byte(name)) {
					t.Fatal("raw email retained in identity shard or shard unreadable")
				}
			})
		}
	}
}
