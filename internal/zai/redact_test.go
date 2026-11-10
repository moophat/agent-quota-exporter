package zai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

// No log line, error, sample, label, event or state blob may ever hold the key, in any common encoding.
func TestKeyNeverLeaves(t *testing.T) {
	sum := sha256.Sum256([]byte(canaryKey))
	needles := []string{canaryKey, "CANARYZAI", base64.StdEncoding.EncodeToString([]byte(canaryKey)),
		base64.RawURLEncoding.EncodeToString([]byte(canaryKey)), hex.EncodeToString([]byte(canaryKey)),
		hex.EncodeToString(sum[:])[:16], base64.StdEncoding.EncodeToString(sum[:])[:12]}
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	peers := &Peers{}
	rt := &fakeRT{}
	var all strings.Builder
	record := func(s *Source, now time.Time, blob []byte) []byte {
		res, out, err := s.Collect(context.Background(), now, blob)
		fmt.Fprintf(&all, "%v|%s|%v|", err, out, res.Errors)
		for _, sm := range res.Samples {
			fmt.Fprintf(&all, "%s%v%v|", sm.Name, sm.Labels, sm.Value)
		}
		return out
	}
	for _, ks := range []string{"openclaw", "opencode", "zai-helper"} {
		s := &Source{Host: "node-a", Alias: "z-" + ks, Account: "zai-main", Dir: holderFor(t, ks, canaryKey), KeySource: ks, Version: "test",
			Interval: 10 * time.Minute, Backoff429: 30 * time.Minute, StaleAfter: 45 * time.Minute, Transport: rt, Peers: peers}
		peers.Add(s)
		now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
		var blob []byte
		for _, h := range []func(*http.Request) (*http.Response, error){
			respond(200, globalBody(t)), respond(401, []byte("bad key "+canaryKey)), respond(429, nil), respond(500, nil),
			respond(200, []byte(`{"code":1000,"msg":"`+canaryKey+`","success":false}`)),
			respond(200, []byte(canaryKey)),
			func(*http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("Get https://api.z.ai: %s", canaryKey)
			},
		} {
			rt.handler = h
			blob = record(s, now, blob)
			now = now.Add(time.Minute)
		}
		s.Now = func() time.Time { return now }
		fmt.Fprintf(&all, "%v", s.Restore(blob))
	}
	all.WriteString(logs.String())
	for _, n := range needles {
		if strings.Contains(all.String(), n) {
			t.Fatalf("output contains an encoding of the key (%d bytes needle)", len(n))
		}
	}
}
