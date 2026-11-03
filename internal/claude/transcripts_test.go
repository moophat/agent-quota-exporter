package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func appendFile(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

type tt struct {
	t   *testing.T
	p   *Profile
	src *TranscriptsSource
	st  json.RawMessage
	now time.Time
}

func newTT(t *testing.T) *tt {
	p := newProfile(t, "a")
	return &tt{t: t, p: p, src: &TranscriptsSource{P: p, Interval: time.Minute}, now: t0.Add(time.Minute)}
}

func (x *tt) collect() map[string]float64 {
	x.t.Helper()
	res, st, err := x.src.Collect(context.Background(), x.now, x.st)
	if err != nil {
		x.t.Fatal(err)
	}
	x.st = st
	noCanary(x.t, res.Samples, err)
	out := map[string]float64{}
	for _, s := range res.Samples {
		var k []string
		for _, l := range s.Labels {
			if l.Name == "model" || l.Name == "type" {
				k = append(k, l.Value)
			}
		}
		out[s.Name+":"+strings.Join(k, ":")] = s.Value
	}
	if res.Errors["parse"] > 0 {
		out["parse"] = float64(res.Errors["parse"])
	}
	return out
}

// restart drops the in-memory source and builds a new one from the persisted blob only.
func (x *tt) restart() { x.src = &TranscriptsSource{P: x.p, Interval: time.Minute} }

func (x *tt) file(name string) string { return filepath.Join(x.p.Dir, "projects", "-root", name) }

func wantFixtureTotals(t *testing.T, got map[string]float64, mult float64) {
	t.Helper()
	want := map[string]float64{
		"agent_tokens_total:model-x:input":       5 * mult,
		"agent_tokens_total:model-x:output":      507 * mult, // 412 (streamed duplicate keeps the highest) + 95
		"agent_tokens_total:model-x:cache_read":  77647 * mult,
		"agent_tokens_total:model-x:cache_write": 26495 * mult,
		"agent_requests_total:model-x":           2 * mult,
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
	for k := range got {
		if strings.Contains(k, "synthetic") {
			t.Fatalf("<synthetic> model counted: %v", got)
		}
	}
}

// History present at first start is never replayed; only what is written afterwards counts.
func TestTranscriptsNoReplayThenCount(t *testing.T) {
	x := newTT(t)
	write(t, x.file("old.jsonl"), fixture(t, "transcript-sample.jsonl"))
	if got := x.collect(); len(got) != 0 {
		t.Fatalf("first run must count nothing, got %v", got)
	}
	appendFile(t, x.file("old.jsonl"), fixture(t, "transcript-sample.jsonl"))
	got := x.collect()
	// The appended copy repeats message ids already... not seen (history was skipped), so it counts once.
	wantFixtureTotals(t, got, 1)
}

func TestTranscriptsNewFileCountedAndDeduped(t *testing.T) {
	x := newTT(t)
	x.collect() // init with no files
	write(t, x.file("s1.jsonl"), fixture(t, "transcript-sample.jsonl"))
	wantFixtureTotals(t, x.collect(), 1)
	// A second file carrying the same message ids (resume/fork copy) adds nothing.
	write(t, filepath.Join(x.p.Dir, "projects", "-root", "subagents", "s2.jsonl"), fixture(t, "transcript-sample.jsonl"))
	wantFixtureTotals(t, x.collect(), 1)
}

// A file that appears later carrying old history (copied on resume) is not counted as new usage.
func TestTranscriptsCopiedHistorySkipped(t *testing.T) {
	x := newTT(t)
	x.now = t0.Add(10 * 24 * time.Hour)
	x.collect()
	write(t, x.file("copy.jsonl"), fixture(t, "transcript-sample.jsonl"))
	if got := x.collect(); got["agent_requests_total:model-x"] != 0 {
		t.Fatalf("copied history counted: %v", got)
	}
}

// After an outage, lines past the stored offset count however old they are (nothing lost).
func TestTranscriptsBacklogAfterOutageCounted(t *testing.T) {
	x := newTT(t)
	write(t, x.file("s.jsonl"), nil)
	x.collect()
	appendFile(t, x.file("s.jsonl"), fixture(t, "transcript-sample.jsonl"))
	x.now = t0.Add(30 * 24 * time.Hour)
	x.restart()
	wantFixtureTotals(t, x.collect(), 1)
}

// Counters continue across a restart without double counting.
func TestTranscriptsRestartContinues(t *testing.T) {
	x := newTT(t)
	x.collect()
	write(t, x.file("s.jsonl"), fixture(t, "transcript-sample.jsonl"))
	x.collect()
	x.restart()
	wantFixtureTotals(t, x.collect(), 1)
	appendFile(t, x.file("s.jsonl"), []byte(`{"type":"assistant","timestamp":"2026-01-01T00:01:00.000Z","message":{"id":"msg_new","model":"model-x","content":[],"usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`+"\n"))
	got := x.collect()
	if got["agent_tokens_total:model-x:input"] != 15 || got["agent_requests_total:model-x"] != 3 {
		t.Fatalf("after restart+append: %v", got)
	}
}

// Deleting the state dir restarts counting at zero: no replay.
func TestTranscriptsStateLostNoReplay(t *testing.T) {
	x := newTT(t)
	x.collect()
	write(t, x.file("s.jsonl"), fixture(t, "transcript-sample.jsonl"))
	x.collect()
	x.restart()
	x.st = nil
	if got := x.collect(); len(got) != 0 {
		t.Fatalf("state loss replayed history: %v", got)
	}
}

// A line still being written (no newline yet) is not consumed until it is complete.
func TestTranscriptsPartialLine(t *testing.T) {
	x := newTT(t)
	write(t, x.file("s.jsonl"), nil)
	x.collect()
	ln := `{"type":"assistant","timestamp":"2026-01-01T00:01:00.000Z","message":{"id":"msg_p","model":"model-x","content":[],"usage":{"input_tokens":7,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`
	appendFile(t, x.file("s.jsonl"), []byte(ln[:40]))
	if got := x.collect(); got["agent_tokens_total:model-x:input"] != 0 || got["parse"] != 0 {
		t.Fatalf("partial line consumed: %v", got)
	}
	appendFile(t, x.file("s.jsonl"), []byte(ln[40:]+"\n"))
	if got := x.collect(); got["agent_tokens_total:model-x:input"] != 7 {
		t.Fatalf("completed line not counted: %v", got)
	}
}

// Truncation or replacement re-reads the file; dedupe keeps the counters from jumping.
func TestTranscriptsTruncationNoDoubleCount(t *testing.T) {
	x := newTT(t)
	x.collect()
	write(t, x.file("s.jsonl"), fixture(t, "transcript-sample.jsonl"))
	x.collect()
	write(t, x.file("s.jsonl"), fixture(t, "transcript-sample.jsonl")[:10]) // truncate
	x.collect()
	write(t, x.file("s.jsonl"), fixture(t, "transcript-sample.jsonl"))
	wantFixtureTotals(t, x.collect(), 1)
}

// Message content and an unsafe model string never reach labels.
func TestTranscriptsContentAndModelBounded(t *testing.T) {
	x := newTT(t)
	x.collect()
	write(t, x.file("s.jsonl"), []byte(`{"type":"assistant","timestamp":"2026-01-01T00:01:00.000Z","message":{"id":"msg_c","model":"CANARY-CONTENT /etc/passwd prompt text","content":[{"type":"text","text":"CANARY-CONTENT secret prompt"}],"usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`+"\n"+`{not json "assistant"`+"\n"))
	got := x.collect()
	if got["agent_requests_total:other"] != 1 || got["parse"] != 1 {
		t.Fatalf("got %v", got)
	}
}

func asstLine(id, model, ts string, in int) []byte {
	return []byte(fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":%q,"model":%q,"content":[],"usage":{"input_tokens":%d,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`+"\n", ts, id, model, in))
}

// The model cap is decided when a message is counted: a model keeps its own series while in use, a new model
// takes the slot of one unused for a day (that series ends) or counts into "other". No total ever moves between
// series, so model="other" never goes down.
func TestTranscriptsModelCapNeverMovesTotals(t *testing.T) {
	x := newTT(t)
	x.p.MaxModels = 1
	x.collect()
	write(t, x.file("s.jsonl"), asstLine("m1", "old-model", "2026-01-01T00:01:00Z", 1))
	x.collect()
	x.now = x.now.Add(time.Minute)
	appendFile(t, x.file("s.jsonl"), asstLine("m2", "new-model", "2026-01-01T00:02:00Z", 1))
	got := x.collect()
	if got["agent_requests_total:old-model"] != 1 || got["agent_requests_total:other"] != 1 {
		t.Fatalf("slot in use must keep its model; new one counts as other: %v", got)
	}
	// A day later the unused old model gives up its slot; other keeps what it had.
	x.now = x.now.Add(25 * time.Hour)
	appendFile(t, x.file("s.jsonl"), asstLine("m3", "new-model", "2026-01-02T01:00:00Z", 1))
	got = x.collect()
	if got["agent_requests_total:new-model"] != 1 || got["agent_requests_total:other"] != 1 {
		t.Fatalf("after a day: %v", got)
	}
	if _, ok := got["agent_requests_total:old-model"]; ok {
		t.Fatalf("evicted model series must end, not move into other: %v", got)
	}
	// The old model coming back while the slot is busy counts into other, which only grows.
	appendFile(t, x.file("s.jsonl"), asstLine("m4", "old-model", "2026-01-02T01:01:00Z", 1))
	if got = x.collect(); got["agent_requests_total:other"] != 2 {
		t.Fatalf("other must only grow: %v", got)
	}
}

// A session file created and written during an exporter outage is counted after the restart (Qodo #4).
func TestTranscriptsFileCreatedDuringOutageCounted(t *testing.T) {
	x := newTT(t)
	x.collect() // last collect at t0+1m
	x.now = x.now.Add(3 * time.Hour)
	x.restart()
	write(t, x.file("new.jsonl"), asstLine("o1", "model-x", "2026-01-01T01:00:00Z", 9)) // written 2h before restart
	if got := x.collect(); got["agent_tokens_total:model-x:input"] != 9 {
		t.Fatalf("outage session lost: %v", got)
	}
}

// A truncated or replaced file re-read from the start does not recount history, even when the dedupe set no
// longer holds those ids (Qodo #5).
func TestTranscriptsReplacedFileNoReplay(t *testing.T) {
	x := newTT(t)
	x.collect()
	write(t, x.file("s.jsonl"), asstLine("r1", "model-x", "2026-01-01T00:01:00Z", 4))
	x.collect()
	x.now = x.now.Add(3 * time.Hour)
	x.collect()                           // last collect well after the line was written
	x.src.st.Seen = map[string]*seenMsg{} // as if evicted
	x.now = x.now.Add(2 * time.Hour)
	// Replace by rename, so the new file has a new inode whatever the filesystem reuses.
	tmp := x.file("s.jsonl.new")
	write(t, tmp, asstLine("r1", "model-x", "2026-01-01T00:01:00Z", 4))
	if err := os.Rename(tmp, x.file("s.jsonl")); err != nil {
		t.Fatal(err)
	}
	if got := x.collect(); got["agent_tokens_total:model-x:input"] != 4 {
		t.Fatalf("replaced file recounted: %v", got)
	}
}

// Truncation in place (same inode, smaller size) is re-read from the start with the same rule.
func TestTranscriptsTruncatedInPlaceNoReplay(t *testing.T) {
	x := newTT(t)
	x.collect()
	write(t, x.file("s.jsonl"), append(asstLine("r1", "model-x", "2026-01-01T00:01:00Z", 4),
		asstLine("r2", "model-x", "2026-01-01T00:02:00Z", 1)...))
	x.collect()
	x.now = x.now.Add(3 * time.Hour)
	x.collect()
	x.src.st.Seen = map[string]*seenMsg{}
	f, err := os.OpenFile(x.file("s.jsonl"), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(asstLine("r1", "model-x", "2026-01-01T00:01:00Z", 4))
	f.Close()
	if got := x.collect(); got["agent_tokens_total:model-x:input"] != 5 {
		t.Fatalf("truncated file recounted: %v", got)
	}
}

// A user-controlled link inside a profile is never followed (Qodo #3).
func TestTranscriptsSymlinkedProjectsRefused(t *testing.T) {
	x := newTT(t)
	os.RemoveAll(filepath.Join(x.p.Dir, "projects"))
	other := t.TempDir()
	os.Symlink(other, filepath.Join(x.p.Dir, "projects"))
	if _, _, err := x.src.Collect(context.Background(), x.now, nil); err == nil || err.Error() != "source error: permission" {
		t.Fatalf("want permission, got %v", err)
	}
}
