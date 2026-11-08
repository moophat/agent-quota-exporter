package claude

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// AI-Model: t9r/cx/gpt-6.1-sol
func ownerLabel(v string) string { return fmt.Sprintf("acct-%x", sha256.Sum256([]byte(v)))[:13] }
func ownerRow(v string) []byte {
	return []byte(fmt.Sprintf("{\"type\":\"session-owner\",\"accountUuid\":%q}\n", v))
}
func ownerMessage(id string, out int, at time.Time) []byte {
	return []byte(fmt.Sprintf("{\"type\":\"assistant\",\"timestamp\":%q,\"message\":{\"id\":%q,\"model\":\"model-x\",\"usage\":{\"output_tokens\":%d}}}\n", at.Format(time.RFC3339Nano), id, out))
}
func ownerTotals(t *testing.T, x *tt) map[string]float64 {
	t.Helper()
	r, st, e := x.src.Collect(context.Background(), x.now, x.st)
	if e != nil {
		t.Fatal(e)
	}
	x.st = st
	out := map[string]float64{}
	for _, s := range r.Samples {
		if s.Name != "agent_tokens_total" {
			continue
		}
		a, typ := "", ""
		for _, l := range s.Labels {
			if l.Name == "account" {
				a = l.Value
			}
			if l.Name == "type" {
				typ = l.Value
			}
		}
		if typ == "output" {
			out[a] += s.Value
		}
	}
	return out
}
func ownerBinding(x *tt, private, metric string, at time.Time) {
	x.p.history = append(x.p.history, identitySample{data: IdentityData{Account: &private, MetricAccount: &metric, IntervalID: private}, at: at, last: at})
}
func TestSessionOwnerA(t *testing.T) {
	x := newTT(t)
	write(t, x.file("a.jsonl"), nil)
	x.collect()
	ownerBinding(x, "private-a", ownerLabel("owner-a"), x.now)
	appendFile(t, x.file("a.jsonl"), append(ownerRow("owner-a"), ownerMessage("m", 10, x.now)...))
	got := ownerTotals(t, x)
	if got[ownerLabel("owner-a")] != 10 {
		t.Fatalf("initial owner A tokens: %v", got)
	}
	ownerBinding(x, "private-b", ownerLabel("owner-b"), x.now.Add(time.Minute))
	x.restart()
	appendFile(t, x.file("a.jsonl"), ownerMessage("m", 15, x.now))
	got = ownerTotals(t, x)
	if got[ownerLabel("owner-a")] != 15 || got[ownerLabel("owner-b")] != 0 {
		t.Fatalf("growth/restart must stay A; got %v", got)
	}
}
func TestSessionOwnerB(t *testing.T) {
	x := newTT(t)
	write(t, x.file("seed.jsonl"), nil)
	x.collect()
	for _, v := range []string{"owner-a", "owner-b"} {
		appendFile(t, x.file(v+".jsonl"), append(ownerRow(v), ownerMessage(v, 7, x.now)...))
	}
	got := ownerTotals(t, x)
	for _, v := range []string{"owner-a", "owner-b"} {
		if got[ownerLabel(v)] != 7 {
			t.Fatalf("independent file owners: %v", got)
		}
	}
}
func TestSessionOwnerC(t *testing.T) {
	x := newTT(t)
	write(t, x.file("headless.jsonl"), nil)
	x.collect()
	ownerBinding(x, "private-a", "metric-a", x.now.Add(-time.Minute))
	appendFile(t, x.file("headless.jsonl"), ownerMessage("m", 9, x.now))
	got := ownerTotals(t, x)
	if got["metric-a"] != 9 {
		t.Fatalf("timestamp binding must own headless usage: %v", got)
	}
	raw, _, err := x.src.transcriptEvent(ownerMessage("m", 9, x.now), "headless.jsonl", &transcriptCursor{}, 0, 1, x.now)
	if err != nil {
		t.Fatal(err)
	}
	var row struct{ Data map[string]any }
	json.Unmarshal(raw, &row)
	if row.Data["account"] != "metric-a" || row.Data["attribution_quality"] != "sampled" {
		t.Fatalf("sampled event: %s", raw)
	}
}
func TestSessionOwnerD(t *testing.T) {
	x := newTT(t)
	write(t, x.file("headless.jsonl"), nil)
	x.collect()
	appendFile(t, x.file("headless.jsonl"), ownerMessage("m", 9, x.now))
	got := ownerTotals(t, x)
	if got["unknown"] != 9 {
		t.Fatalf("no evidence must be unknown, never current login: %v", got)
	}
}
func TestSessionOwnerE(t *testing.T) {
	x := newTT(t)
	padding := strings.Repeat("{\"type\":\"progress\"}\n", 100000)
	write(t, x.file("deep.jsonl"), append([]byte(padding), append(ownerRow("owner-a"), []byte(padding)...)...))
	x.collect()
	x.restart()
	appendFile(t, x.file("deep.jsonl"), ownerMessage("m", 11, x.now))
	got := ownerTotals(t, x)
	if got[ownerLabel("owner-a")] != 11 {
		t.Fatalf("deep owner at EOF must survive restart: %v", got)
	}
}

func TestSessionOwnerForwardChangesAndCaptureRestart(t *testing.T) {
	x, store := recoverySource(t)
	path := x.file("owners.jsonl")
	write(t, path, append(ownerRow("owner-a"), ownerMessage("one", 4, x.now)...))
	x.collect()
	dir := x.src.StateDir
	x.restart()
	x.src.StateDir = dir
	appendFile(t, path, append(ownerMessage("two", 5, x.now), append(ownerRow("owner-b"), ownerMessage("three", 6, x.now)...)...))
	got := ownerTotals(t, x)
	if got[ownerLabel("owner-a")] != 5 || got[ownerLabel("owner-b")] != 6 {
		t.Fatalf("forward owner counters: %v", got)
	}
	var accounts []string
	err := store.Replay(func(raw json.RawMessage) error {
		var row struct {
			Kind string
			Data struct {
				Account  string
				Evidence map[string]string `json:"account_evidence"`
				Quality  string            `json:"attribution_quality"`
			}
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		if row.Kind == "usage" {
			if row.Data.Evidence["comparison"] != "session_owner" || row.Data.Quality != "embedded" {
				t.Fatalf("owner evidence: %s", raw)
			}
			accounts = append(accounts, row.Data.Account)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(accounts, ",") != strings.Join([]string{ownerLabel("owner-a"), ownerLabel("owner-a"), ownerLabel("owner-b")}, ",") {
		t.Fatalf("durable forward owner accounts: %v", accounts)
	}
}

func TestSessionOwnerBackwardLatestAndCap(t *testing.T) {
	x := newTT(t)
	path := x.file("scan.jsonl")
	write(t, path, append(ownerRow("owner-a"), append([]byte(strings.Repeat("{\"type\":\"progress\"}\n", 4000)), ownerRow("owner-b")...)...))
	f, fi, err := openRegular(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got, err := recoverTranscriptOwner(context.Background(), f, fi.Size()); err != nil || got != "owner-b" {
		t.Fatalf("latest backward owner: %q", got)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// Sparse padding proves the cap without allocating a 64 MiB fixture.
	fw, err := os.OpenFile(path, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := fw.Truncate(65 << 20); err != nil {
		t.Fatal(err)
	}
	fw.Close()
	f, fi, err = openRegular(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got, err := recoverTranscriptOwner(context.Background(), f, fi.Size()); err != nil || got != "" {
		t.Fatalf("owner beyond cap: %q", got)
	}
}
