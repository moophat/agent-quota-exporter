package claude

// AI-Model: t9r/cx/gpt-6.1-sol
import (
	"encoding/json"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/events"
)

func TestUsageMetricAccountStored(t *testing.T) {
	for _, tc := range []struct {
		name, owner, alias, currentUUID, binding string
		want                                     any
	}{
		{name: "session owner", owner: "owner-a", want: ownerLabel("owner-a")},
		{name: "verified owner alias", owner: "owner-a", currentUUID: "owner-a", alias: "acct-alias", want: "acct-alias"},
		{name: "different owner ignores alias", owner: "owner-a", currentUUID: "owner-b", alias: "acct-alias", want: ownerLabel("owner-a")},
		{name: "identity binding", binding: "historical-label", want: "historical-label"},
		{name: "unattributed", want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, store := recoverySource(t)
			x.p.AccountAlias = tc.alias
			// Freeze the same owner snapshot both metric and event attribution use.
			x.p.ownerSnapshotReady = true
			x.p.ownerLabel, x.p.ownerUUID = tc.alias, tc.currentUUID
			if tc.binding != "" {
				private := "private-keyed-account"
				x.p.ownerHistory = []identitySample{{data: IdentityData{Account: &private, MetricAccount: &tc.binding, IntervalID: "interval-a"}, at: x.now.Add(-time.Minute), last: x.now}}
			}
			raw, _, err := x.src.transcriptEvent(ownerMessage("message-a", 9, x.now), "usage.jsonl", &transcriptCursor{Owner: tc.owner}, 0, 1, x.now)
			if err != nil {
				t.Fatal(err)
			}
			var row struct{ Data map[string]any }
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatal(err)
			}
			if got, exists := row.Data["metric_account"]; !exists || got != tc.want {
				t.Errorf("metric_account = %v (present %v), want %v", got, exists, tc.want)
			}
			if err := store.Append(events.Batch{Envelopes: []json.RawMessage{raw}}); err != nil {
				t.Fatalf("store rejected usage: %v", err)
			}
			seen := 0
			if err := store.Replay(func(saved json.RawMessage) error {
				seen++
				var stored struct{ Data map[string]any }
				if err := json.Unmarshal(saved, &stored); err != nil {
					return err
				}
				if got, exists := stored.Data["metric_account"]; !exists || got != tc.want {
					t.Errorf("stored metric_account = %v (present %v), want %v", got, exists, tc.want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if seen != 1 {
				t.Fatalf("stored %d events, want 1", seen)
			}
		})
	}
}
