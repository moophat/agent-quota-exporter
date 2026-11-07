package claude

import (
	"context"
	"encoding/json"
	"github.com/moophat/agent-quota-exporter/internal/core"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPhysicalObservationUsesFullDigest(t *testing.T) {
	e := projectedEvent(t, asstLine("one", "model-x", "2026-01-01T00:00:00Z", 1))
	id := e["event_id"].(string)
	if !strings.HasPrefix(id, "v2:") || len(id) != 67 {
		t.Fatalf("short observation identity: %s", id)
	}
}

func TestUsageQualityNamesAttributionGaps(t *testing.T) {
	e := projectedEvent(t, asstLine("one", "model-x", "2026-01-01T00:00:00Z", 1))
	missing := e["quality"].(map[string]any)["missing_fields"].([]any)
	found := map[any]bool{}
	for _, v := range missing {
		found[v] = true
	}
	if !found["account"] || !found["project"] {
		t.Fatal("attribution gaps absent from quality")
	}
}

func TestInvalidTokenArithmeticIsQualified(t *testing.T) {
	row := []byte(`{"type":"assistant","message":{"id":"one","usage":{"input_tokens":-1,"cache_creation_input_tokens":2,"cache_creation":{"ephemeral_1h_input_tokens":3}}}}`)
	e := projectedEvent(t, row)
	d := e["data"].(map[string]any)
	if d["tokens"].(map[string]any)["input"] != nil {
		t.Fatal("negative normalized input retained")
	}
	if d["accounting_quality"].(map[string]any)["invalid_arithmetic"] != true {
		t.Fatal("invalid arithmetic unqualified")
	}
}

func TestCaptureContinuesAfterChildOpenFailure(t *testing.T) {
	x, store := recoverySource(t)
	write(t, x.file("good.jsonl"), asstLine("one", "model-x", "2026-01-01T00:00:00Z", 1))
	// Inject the same failure produced by a removed directory after ReadDir.
	x.src.captureOpenSubdir = func(d *os.File, name string) (*os.File, error) {
		if name == "broken" {
			return nil, os.ErrPermission
		}
		return openSubdir(d, name)
	}
	os.MkdirAll(filepath.Join(x.p.Dir, "projects", "broken"), 0700)
	res := core.Result{}
	x.src.captureTranscripts(context.Background(), time.Now(), &res)
	if usageRows(t, store) != 1 {
		t.Fatal("child failure blocked readable sibling")
	}
	if x.src.capture.LastComplete != 0 {
		t.Fatal("incomplete discovery reported complete")
	}
}

func TestMetricReadTurnsRotate(t *testing.T) {
	x := newTT(t)
	older := x.file("older.jsonl")
	busy := x.file("busy.jsonl")
	write(t, older, nil)
	write(t, busy, nil)
	x.collect()
	row := asstLine("old", "model-x", "2026-01-01T00:00:00Z", 1)
	appendFile(t, older, row)
	os.Chtimes(older, time.Unix(1, 0), time.Unix(1, 0))
	x.src.ByteBudget = int64(len(row))
	for i := 0; i < 3; i++ {
		appendFile(t, busy, row)
		x.collect()
	}
	if x.src.st.Files[hashKey("-root/older.jsonl")].Off == 0 {
		t.Fatal("older file starved")
	}
}

func TestInterruptedDiscoveryRetainsOffsets(t *testing.T) {
	x, _ := recoverySource(t)
	write(t, x.file("retained.jsonl"), asstLine("one", "model-x", "2026-01-01T00:00:00Z", 1))
	x.collect()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	x.src.Collect(ctx, time.Now(), nil)
	if len(x.src.st.Files) != 1 {
		t.Fatal("interrupted discovery discarded offsets")
	}
}

func TestUnsafeTranscriptStringsDoNotBlockCapture(t *testing.T) {
	x, store := recoverySource(t)
	row := asstLine("one", "model-x", "2026-01-01T00:00:00Z", 1)
	var obj map[string]any
	json.Unmarshal(row, &obj)
	obj["gitBranch"] = "feature/@work"
	row, _ = json.Marshal(obj)
	write(t, x.file("branch.jsonl"), append(row, '\n'))
	x.collect()
	if usageRows(t, store) != 1 {
		t.Fatal("unsafe branch blocked capture")
	}
}

func projectedEvent(t *testing.T, row []byte) map[string]any {
	t.Helper()
	x, _ := recoverySource(t)
	b, _, err := x.src.transcriptEvent(row, "session.jsonl", &transcriptCursor{}, 0, int64(len(row)), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var e map[string]any
	if err = json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestCaptureProgressWithMetricBacklog(t *testing.T) {
	x, store := recoverySource(t)
	p := x.file("busy.jsonl")
	write(t, p, nil)
	x.collect()
	row := asstLine("one", "model-x", "2026-01-01T00:00:00Z", 1)
	x.src.ByteBudget = int64(len(row))
	appendFile(t, p, row)
	x.collect()
	if usageRows(t, store) != 1 {
		t.Fatal("metric reads starved capture")
	}
}
