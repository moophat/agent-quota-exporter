package claude

// AI-Model: t9r/cx/gpt-6.1-sol
import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

func TestOwnerlessCollectionSnapshot(t *testing.T) {
	x, _ := recoverySource(t)
	write(t, filepath.Join(x.p.Dir, ".credentials.json"), []byte(identityFixture))
	x.p.IdentityKey = []byte(strings.Repeat("k", 32))
	ownerBinding(x, "private-a", "metric-a", x.now.Add(-time.Minute))
	x.collect()
	// New samples must not change the shared counter/capture view mid-collection.
	ownerBinding(x, "private-b", "metric-b", x.now)
	// A durable observation arriving after snapshot creation cannot affect either
	// pass. Replaying the event store per message would expose this new sample.
	if err := x.p.SampleIdentity(x.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		metric, _, _, _, _ := x.src.messageOwner(x.now.Add(time.Second).Format(time.RFC3339Nano), "")
		if metric != "metric-a" {
			t.Fatalf("collection snapshot changed during repeated resolution: %s", metric)
		}
	}
	ownerBinding(x, "private-b", "metric-b", x.now.Add(2*time.Second))
	x.collect()
	metric, _, _, _, _ := x.src.messageOwner(x.now.Add(2*time.Second).Format(time.RFC3339Nano), "")
	if metric != "metric-b" {
		t.Fatalf("next collection did not refresh: %s", metric)
	}
}

func TestOwnerRecoveryReadErrorAndEmpty(t *testing.T) {
	x := newTT(t)
	path := x.file("empty.jsonl")
	write(t, path, []byte("{\"type\":\"progress\"}\n"))
	f, fi, err := openRegular(path)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := recoverTranscriptOwner(context.Background(), f, fi.Size())
	if err != nil || owner != "" {
		t.Fatalf("completed empty search: %q %v", owner, err)
	}
	f.Close()
	if _, err := recoverTranscriptOwner(context.Background(), f, fi.Size()); err == nil {
		t.Fatal("closed-file recovery reported completion")
	}
}

func TestCaptureOwnerRecoveryRetries(t *testing.T) {
	x, store := recoverySource(t)
	path := x.file("capture.jsonl")
	prefix := ownerRow("owner-a")
	write(t, path, append(prefix, ownerMessage("m", 7, x.now)...))
	x.src.ByteBudget = int64(len(prefix))
	x.collect()
	cur := x.src.capture.Files[hashKey("capture.jsonl")]
	// The helper stores files under a project directory; select its sole cursor.
	for _, c := range x.src.capture.Files {
		cur = c
	}
	if cur == nil {
		t.Fatal("missing capture cursor")
	}
	cur.Owner, cur.OwnerReady = "", false
	x.src.ByteBudget = 1 << 20
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	x.src.captureTranscripts(ctx, x.now, &core.Result{})
	if cur.OwnerReady || usageRows(t, store) != 0 {
		t.Fatal("interrupted capture advanced attribution")
	}
	x.src.captureTranscripts(context.Background(), x.now, &core.Result{})
	if !cur.OwnerReady || cur.Owner != "owner-a" || usageRows(t, store) != 1 {
		t.Fatalf("capture retry failed: %+v", cur)
	}
}

func TestEmbeddedOwnerVerifiedAlias(t *testing.T) {
	x := newTT(t)
	x.p.ConfigDir = filepath.Join(x.p.Dir, "explicit-profile")
	x.p.AccountAlias = "configured-a"
	write(t, filepath.Join(x.p.Dir, ".claude.json"), []byte(`{"oauthAccount":{"accountUuid":"owner-a"}}`))
	x.collect()
	for _, tc := range []struct{ owner, want string }{{"owner-a", "configured-a"}, {"owner-b", ownerLabel("owner-b")}} {
		got, _, _, _, _ := x.src.messageOwner("", tc.owner)
		if got != tc.want {
			t.Errorf("owner %s: got %s, want %s", tc.owner, got, tc.want)
		}
	}
	if err := os.Remove(filepath.Join(x.p.Dir, ".claude.json")); err != nil {
		t.Fatal(err)
	}
	x.collect()
	got, _, _, _, _ := x.src.messageOwner("", "owner-a")
	if got != ownerLabel("owner-a") {
		t.Fatalf("unverified owner received alias: %s", got)
	}
}

func TestSampledOwnerEmailVerification(t *testing.T) {
	x := newTT(t)
	x.p.EmailLabel = true
	x.p.IdentityKey = []byte(strings.Repeat("k", 32))
	path := filepath.Join(x.p.Dir, ".claude.json")
	write(t, path, []byte(`{"oauthAccount":{"accountUuid":"owner-a","emailAddress":"acct-a@example.com"}}`))
	ownerBinding(x, x.p.opaqueEmail("acct-a@example.com"), "", x.now.Add(-time.Minute))
	x.p.history[0].data.MetricAccount = nil
	x.collect()
	got, _, _, _, _ := x.src.messageOwner(x.now.Format(time.RFC3339Nano), "")
	if got != "acct-a@example.com" {
		t.Errorf("verified current sampled email: got %s", got)
	}
	write(t, path, []byte(`{"oauthAccount":{"accountUuid":"owner-b","emailAddress":"acct-b@example.com"}}`))
	if err := os.Chtimes(path, x.now.Add(time.Hour), x.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	x.collect()
	got, _, _, _, _ = x.src.messageOwner(x.now.Format(time.RFC3339Nano), "")
	if got != "unknown" {
		t.Errorf("historical sample received current email: %s", got)
	}
}

func TestInterruptedOwnerRecoveryRetries(t *testing.T) {
	x := newTT(t)
	x.collect()
	path := x.file("retry.jsonl")
	prefix := ownerRow("owner-a")
	write(t, path, append(prefix, ownerMessage("m", 7, x.now)...))
	fst := &fileState{Off: int64(len(prefix))}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, _ := x.src.readFile(ctx, path, fst, "unknown", x.now)
	if fst.OwnerReady || n != 0 {
		t.Errorf("interrupted recovery committed readiness/progress: ready=%v bytes=%d", fst.OwnerReady, n)
	}
	n, _ = x.src.readFile(context.Background(), path, fst, "unknown", x.now)
	if !fst.OwnerReady || fst.Owner != "owner-a" || n == 0 {
		t.Fatalf("retry failed to recover owner before reading: ready=%v owner=%q bytes=%d", fst.OwnerReady, fst.Owner, n)
	}
}

func TestMetricInitializationRetriesUnreadyOwner(t *testing.T) {
	x := newTT(t)
	path := x.file("initial.jsonl")
	write(t, path, ownerRow("owner-a"))
	x.collect()
	// Persisted initialization can have an EOF cursor but an incomplete search.
	for _, fst := range x.src.st.Files {
		fst.Owner, fst.OwnerReady = "", false
	}
	appendFile(t, path, ownerMessage("next", 8, x.now))
	got := ownerTotals(t, x)
	if got[ownerLabel("owner-a")] != 8 {
		t.Fatalf("initialization retry attributed incorrectly: %v", got)
	}
}
