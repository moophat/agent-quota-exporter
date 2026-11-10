package zai

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// pair builds two profiles on one fake transport; aliases decide ownership.
func pair(t *testing.T, aliasA, ksA, keyA, aliasB, ksB, keyB string) (*Source, *Source, *fakeRT, *Peers) {
	t.Helper()
	rt := &fakeRT{handler: respond(200, globalBody(t))}
	peers := &Peers{}
	mk := func(alias, ks, key string) *Source {
		s := &Source{Host: "node-a", Alias: alias, Account: "zai-main", Dir: holderFor(t, ks, key), KeySource: ks, Version: "test",
			Interval: 10 * time.Minute, Backoff429: 30 * time.Minute, StaleAfter: 45 * time.Minute, Transport: rt, Peers: peers}
		peers.Add(s)
		return s
	}
	return mk(aliasA, ksA, keyA), mk(aliasB, ksB, keyB), rt, peers
}

var peersNow = time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

func collectOnce(t *testing.T, s *Source) int {
	t.Helper()
	res, _, err := s.Collect(context.Background(), peersNow, nil)
	if err != nil {
		t.Fatalf("%s: %v", s.Alias, err)
	}
	return len(res.Samples)
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestDuplicateKeyIsReadOnce(t *testing.T) {
	logs := captureLogs(t)
	// "b-open" is registered first, but the smallest alias owns the key.
	b, a, rt, _ := pair(t, "b-open", "openclaw", canaryKey, "a-code", "opencode", canaryKey)
	if n := collectOnce(t, b); n != 0 {
		t.Fatalf("non-owner emitted %d samples", n)
	}
	if rt.count() != 0 {
		t.Fatal("non-owner made a request")
	}
	if n := collectOnce(t, a); n == 0 {
		t.Fatal("owner emitted nothing")
	}
	if rt.count() != 1 {
		t.Fatalf("%d requests for one account", rt.count())
	}
	collectOnce(t, b)
	collectOnce(t, b)
	if c := strings.Count(logs.String(), "shares its key"); c != 1 {
		t.Fatalf("want one warning, got %d: %s", c, logs.String())
	}
	if s := logs.String(); !strings.Contains(s, "a-code") || !strings.Contains(s, "b-open") || strings.Contains(s, "CANARY") {
		t.Fatalf("warning must name the aliases only: %s", s)
	}
}

func TestDifferentKeysBothPoll(t *testing.T) {
	a, b, rt, _ := pair(t, "a", "openclaw", keyA, "b", "zai-helper", keyB)
	collectOnce(t, a)
	collectOnce(t, b)
	if rt.count() != 2 {
		t.Fatalf("%d requests", rt.count())
	}
}

func TestOwnershipFollowsRotationAndUnreadablePeers(t *testing.T) {
	a, b, rt, _ := pair(t, "a", "openclaw", keyA, "b", "zai-helper", keyA)
	if collectOnce(t, b) != 0 {
		t.Fatal("b should defer to a")
	}
	// a's key rotates: b is now the only holder of keyA and polls
	os.WriteFile(filepath.Join(a.Dir, "openclaw.json"), []byte(`{"models":{"providers":{"zai":{"apiKey":"`+keyC+`"}}}}`), 0o600)
	if collectOnce(t, b) == 0 || rt.count() != 1 {
		t.Fatal("ownership not recomputed")
	}
	// a becomes unreadable: ignored in the comparison
	os.Remove(filepath.Join(a.Dir, "openclaw.json"))
	if collectOnce(t, b) == 0 {
		t.Fatal("unreadable peer blocked the other profile")
	}
	// a returns with b's key: a (smaller alias) owns it again
	os.WriteFile(filepath.Join(a.Dir, "openclaw.json"), []byte(`{"models":{"providers":{"zai":{"apiKey":"`+keyA+`"}}}}`), 0o600)
	before := rt.count()
	if collectOnce(t, b) != 0 || rt.count() != before {
		t.Fatal("b must defer again")
	}
}

func TestPeersConcurrentSafe(t *testing.T) {
	a, b, _, _ := pair(t, "a", "openclaw", keyA, "b", "zai-helper", keyA)
	var wg sync.WaitGroup
	for _, s := range []*Source{a, b, a, b} {
		wg.Add(1)
		go func() { defer wg.Done(); s.Collect(context.Background(), peersNow, nil) }()
	}
	wg.Wait()
}

func TestRestoreSilentForNonOwner(t *testing.T) {
	a, b, _, _ := pair(t, "a", "openclaw", keyA, "b", "zai-helper", keyA)
	_, blobA, _ := a.Collect(context.Background(), peersNow, nil)
	a.Now = func() time.Time { return peersNow }
	b.Now = a.Now
	if len(a.Restore(blobA).Samples) == 0 {
		t.Fatal("owner restore empty")
	}
	if len(b.Restore(blobA).Samples) != 0 {
		t.Fatal("non-owner restored samples")
	}
}
