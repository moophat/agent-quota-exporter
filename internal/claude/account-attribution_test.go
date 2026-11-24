package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/events"
)

func TestIncidentalSecretMarker(t *testing.T) {
	x, store := recoverySource(t)
	x.p.EmailLabel = true
	write(t, filepath.Join(x.p.Dir, ".credentials.json"), []byte(strings.ReplaceAll(identityFixture, "acct-a@example.com", "risk-ops@example.com")))
	write(t, filepath.Join(x.p.Dir, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"risk-ops@example.com","accountUuid":"owner-a"}}`))
	if err := x.p.SampleIdentity(x.now); err != nil {
		t.Fatal(err)
	}
	x.p.refreshOwnerSnapshot()
	raw, _, err := x.src.transcriptEvent(ownerMessage("message-a", 9, x.now), "usage.jsonl", &transcriptCursor{}, 0, 1, x.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(events.Batch{Envelopes: []json.RawMessage{raw}}); err != nil {
		t.Fatal(err)
	}
	if err := x.p.SampleIdentity(x.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if x.p.pending != nil {
		t.Fatal("identity remains pending")
	}
	seen := 0
	if err := store.Replay(func(raw json.RawMessage) error {
		var row struct{ Data map[string]any }
		json.Unmarshal(raw, &row)
		if row.Data["account"] != "risk-ops@example.com" {
			t.Errorf("account: %v", row.Data["account"])
		}
		seen++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 3 {
		t.Fatalf("stored %d rows, want 3", seen)
	}
}

func TestHistoricalAccountJoin(t *testing.T) {
	for _, metric := range []string{"public-label", ""} {
		t.Run(metric, func(t *testing.T) {
			x, store := recoverySource(t)
			write(t, filepath.Join(x.p.Dir, ".credentials.json"), []byte(identityFixture))
			if err := x.p.SampleIdentity(x.now); err != nil {
				t.Fatal(err)
			}
			var historical json.RawMessage
			if err := store.Replay(func(raw json.RawMessage) error { historical = append(json.RawMessage(nil), raw...); return nil }); err != nil {
				t.Fatal(err)
			}
			var row map[string]any
			json.Unmarshal(historical, &row)
			data := row["data"].(map[string]any)
			data["account"] = "legacy-keyed-account"
			if metric == "" {
				delete(data, "metric_account")
			} else {
				data["metric_account"] = metric
			}
			row["event_id"] = "historical-row"
			raw, _ := json.Marshal(row)
			legacy, err := events.Open(events.Options{Root: t.TempDir(), Budget: 1 << 20, Reserve: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer legacy.Close()
			if err := legacy.Append(events.Batch{Envelopes: []json.RawMessage{raw}}); err != nil {
				t.Fatal(err)
			}
			p := &Profile{Host: x.p.Host, Alias: x.p.Alias, Events: legacy}
			s := &TranscriptsSource{P: p}
			gotMetric, account, _, _, quality := s.messageOwner(x.now.Format(time.RFC3339Nano), "")
			wantMetric := metric
			if metric == "" {
				wantMetric = "unknown"
			}
			if account != "legacy-keyed-account" || gotMetric != wantMetric || quality != "sampled" {
				t.Fatalf("historical join account=%v metric=%s quality=%s", account, gotMetric, quality)
			}
			usage, _, err := s.transcriptEvent(ownerMessage("legacy-message", 9, x.now), "usage.jsonl", &transcriptCursor{}, 0, 1, x.now)
			if err != nil {
				t.Fatal(err)
			}
			var event struct{ Data map[string]any }
			if err := json.Unmarshal(usage, &event); err != nil {
				t.Fatal(err)
			}
			var want any
			if metric != "" {
				want = metric
			}
			if event.Data["account"] != "legacy-keyed-account" || event.Data["metric_account"] != want {
				t.Fatalf("historical usage: %s", usage)
			}
			if err := legacy.Append(events.Batch{Envelopes: []json.RawMessage{usage}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUnreadableCredentials(t *testing.T) {
	for _, kind := range []string{"missing", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			p := identityProfile(t, "profile-a")
			store, err := events.Open(events.Options{Root: t.TempDir(), Budget: 1 << 20, Reserve: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			p.Events = store
			write(t, filepath.Join(p.Dir, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"acct-a@example.com","accountUuid":"owner-a"}}`))
			path := filepath.Join(p.Dir, ".credentials.json")
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if kind == "directory" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink" {
				if err := os.Symlink(".claude.json", path); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.SampleIdentity(t0); err != nil {
				t.Fatal(err)
			}
			b := p.IdentityAt(t0)
			if b.Account != nil || b.MetricAccount != nil {
				t.Fatalf("unreadable credentials attributed: %+v", b)
			}
			var row struct {
				Quality struct {
					Status  string
					Reasons []string
				}
			}
			if err := store.Replay(func(raw json.RawMessage) error { return json.Unmarshal(raw, &row) }); err != nil {
				t.Fatal(err)
			}
			if row.Quality.Status != "unknown" || len(row.Quality.Reasons) != 1 || row.Quality.Reasons[0] != "credential_unreadable" {
				t.Fatalf("quality: %+v", row.Quality)
			}
		})
	}
}

func TestConflictingIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, email, uuid string
		known             bool
	}{
		{"email mismatch", "other@example.com", "owner-a", false},
		{"UUID mismatch", "acct-a@example.com", "owner-b", false},
		{"same identity", "acct-a@example.com", "owner-a", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := identityProfile(t, "profile-a")
			write(t, filepath.Join(p.Dir, ".credentials.json"), []byte(`{"oauthAccount":{"emailAddress":"acct-a@example.com","accountUuid":"owner-a","organizationName":"org-a","organizationType":"team","billingType":"card","seatTier":"pro","hasExtraUsageEnabled":false,"organizationRateLimitTier":"tier-a","userRateLimitTier":"tier-a"}}`))
			b, _ := json.Marshal(map[string]any{"oauthAccount": map[string]any{"emailAddress": tc.email, "accountUuid": tc.uuid}})
			write(t, filepath.Join(p.Dir, ".claude.json"), b)
			if err := p.SampleIdentity(t0); err != nil {
				t.Fatal(err)
			}
			binding := p.IdentityAt(t0)
			if (binding.Account != nil) != tc.known {
				t.Fatalf("conflict attribution: %+v", binding)
			}
		})
	}
}
