package codex

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
)

// The whole fixture tree, appearing after the first collect, read from the start. Expected values by hand from
// fixtures/codex/home (Codex nests cached and cache-write input inside input_tokens and reasoning inside
// output_tokens; the contract's types are disjoint):
//
//	day 3: turn_context gpt-5-codex; A {in 1000, cached 600, cw 100, out 200, rs 50} written twice with the same
//	       total (counted once); turn_context GPT-5; B {500, 300, 0, 100, 40} with rate_limits null.
//	day 4: C {50, 0, 0, 10, 0} before any turn_context (model unknown); turn_context gpt-5-codex;
//	       D {2000, 1500, 200, 300, 120} written twice; E {10, 0, 0, 5, 0} with rate_limits null.
//
//	gpt-5-codex = A + D + E: input (1000-600-100)+(2000-1500-200)+10 = 610, output (200-50)+(300-120)+5 = 335,
//	              cache_read 600+1500 = 2100, cache_write 100+200 = 300, reasoning 50+120 = 170, requests 3
//	gpt-5 = B:    input 500-300 = 200, output 100-40 = 60, cache_read 300, cache_write 0, reasoning 40, requests 1
//	unknown = C:  input 50, output 10, requests 1
//
// Quota is D's (the newest event with rate_limits; E's null never overwrites it), see quotaD.
var goldenCounters = merge(
	tokens("gpt-5-codex", 610, 335, 2100, 300, 170, 3),
	tokens("gpt-5", 200, 60, 300, 0, 40, 1),
	tokens("unknown", 50, 10, 0, 0, 0, 1),
)

func TestGoldenFixtureTree(t *testing.T) {
	h := newHarness(t)
	h.now = tStart
	if got := h.collect(); len(got) != 0 {
		t.Fatalf("empty sessions: %v", got)
	}
	h.copyTree()
	h.now = tNow
	equal(t, "golden", h.collect(), merge(goldenCounters, quotaD))
	if h.parse != 0 {
		t.Fatalf("parse errors: %d", h.parse)
	}
	// Nothing new: counters stay, nothing is counted twice.
	h.now = tNow.Add(time.Minute)
	equal(t, "second collect", h.collect(), merge(goldenCounters, quotaD))
}

// History present at first start is never counted, but its quota is served at once (idle profile); a repeat of
// the last pre-existing event is not counted, a new turn is, under the model the scan found; a restart from the
// persisted state continues without double counting.
func TestNoReplayIdleQuotaThenNewLines(t *testing.T) {
	h := newHarness(t)
	h.copyTree()
	got := h.collect()
	if len(counters(got)) != 0 {
		t.Fatalf("history replayed: %v", counters(got))
	}
	equal(t, "quota from history", quota(got), quotaD)

	eventE := tokenLine(time.Date(2026, 11, 4, 9, 10, 0, 0, time.UTC), tu{10, 0, 0, 5, 0}, tu{2060, 1500, 200, 315, 120}, "null")
	appendFile(t, h.path(fixtureDay4), eventE)
	h.now = tNow.Add(time.Minute)
	if got := h.collect(); len(counters(got)) != 0 {
		t.Fatalf("repeat of a pre-existing event counted: %v", counters(got))
	}

	appendFile(t, h.path(fixtureDay4), tokenLine(tNow.Add(2*time.Minute), tu{100, 40, 0, 20, 5}, tu{2160, 1540, 200, 335, 125}, "null"))
	h.now = tNow.Add(3 * time.Minute)
	want := tokens("gpt-5-codex", 60, 15, 40, 0, 5, 1)
	equal(t, "new turn", counters(h.collect()), want)

	h.restart()
	h.now = tNow.Add(4 * time.Minute)
	equal(t, "after restart", counters(h.collect()), want)
	appendFile(t, h.path(fixtureDay4), tokenLine(tNow.Add(5*time.Minute), tu{10, 0, 0, 1, 0}, tu{2170, 1540, 200, 336, 125}, "null"))
	h.now = tNow.Add(6 * time.Minute)
	equal(t, "after restart and a turn", counters(h.collect()), tokens("gpt-5-codex", 70, 16, 40, 0, 5, 2))
}

// Losing the state restarts counting at zero: no replay.
func TestStateLostNoReplay(t *testing.T) {
	h := newHarness(t)
	h.now = tStart
	h.collect()
	h.copyTree()
	h.now = tNow
	h.collect()
	h.restart()
	h.st = nil
	if got := h.collect(); len(counters(got)) != 0 {
		t.Fatalf("state loss replayed history: %v", counters(got))
	}
}

// A file that appears later carrying old history (fork or resume copy) is not new usage; a line inside the grace
// hour (writer/reader clock skew) is.
func TestCopiedHistorySkipped(t *testing.T) {
	h := newHarness(t)
	h.collect()
	write(t, h.path("2026/11/04/rollout-copy.jsonl"), append(
		tokenLine(tNow.Add(-3*time.Hour), tu{1000, 0, 0, 0, 0}, tu{1000, 0, 0, 0, 0}, "null"),
		tokenLine(tNow.Add(-30*time.Minute), tu{7, 0, 0, 0, 0}, tu{1007, 0, 0, 0, 0}, "null")...))
	h.now = tNow.Add(time.Minute)
	equal(t, "copied history", counters(h.collect()), tokens("unknown", 7, 0, 0, 0, 0, 1))
}

// A replaced file is read again from the start as a new file: its old lines are not counted again.
func TestReplacedFileNoReplay(t *testing.T) {
	h := newHarness(t)
	h.collect()
	line := tokenLine(tNow.Add(time.Minute), tu{4, 0, 0, 0, 0}, tu{4, 0, 0, 0, 0}, "null")
	write(t, h.path("2026/11/04/rollout-r.jsonl"), line)
	h.now = tNow.Add(2 * time.Minute)
	h.collect()
	h.now = tNow.Add(5 * time.Hour)
	h.collect()
	tmp := h.path("2026/11/04/rollout-r.jsonl.new")
	write(t, tmp, line)
	if err := os.Rename(tmp, h.path("2026/11/04/rollout-r.jsonl")); err != nil {
		t.Fatal(err)
	}
	h.now = tNow.Add(6 * time.Hour)
	equal(t, "replaced", counters(h.collect()), tokens("unknown", 4, 0, 0, 0, 0, 1))
}

// A line still being written is not consumed until it is complete.
func TestPartialLine(t *testing.T) {
	h := newHarness(t)
	h.collect()
	p := h.path("2026/11/04/rollout-p.jsonl")
	write(t, p, nil)
	h.collect()
	ln := tokenLine(tNow.Add(time.Minute), tu{9, 0, 0, 1, 0}, tu{9, 0, 0, 1, 0}, "null")
	appendFile(t, p, ln[:50])
	if got := h.collect(); len(counters(got)) != 0 || h.parse != 0 {
		t.Fatalf("partial line consumed: %v parse=%d", got, h.parse)
	}
	appendFile(t, p, ln[50:])
	equal(t, "completed", counters(h.collect()), tokens("unknown", 9, 1, 0, 0, 0, 1))
}

// An over-long relevant line is skipped and counted as a parse error, an over-long irrelevant line (tool output)
// is skipped silently, and the lines after both are counted.
func TestOverlongLines(t *testing.T) {
	h := newHarness(t)
	h.collect()
	pad := strings.Repeat("x", maxLine+1)
	var b bytes.Buffer
	b.WriteString(`{"timestamp":"2026-11-04T12:00:30Z","type":"event_msg","payload":{"type":"token_count","pad":"` + pad + `","info":null,"rate_limits":null}}` + "\n")
	b.WriteString(`{"timestamp":"2026-11-04T12:00:31Z","type":"response_item","payload":{"type":"function_call_output","output":"` + pad + `"}}` + "\n")
	b.Write(tokenLine(tNow.Add(time.Minute), tu{3, 0, 0, 1, 0}, tu{3, 0, 0, 1, 0}, "null"))
	write(t, h.path("2026/11/04/rollout-long.jsonl"), b.Bytes())
	h.now = tNow.Add(2 * time.Minute)
	equal(t, "after long lines", counters(h.collect()), tokens("unknown", 3, 1, 0, 0, 0, 1))
	if h.parse != 1 {
		t.Fatalf("parse errors = %d, want 1 (the relevant over-long line only)", h.parse)
	}
}

// Content never reaches a label, a value, an error or the persisted state, even from a path, a model field or a
// line that fails to parse.
func TestContentCanaryNeverLeaks(t *testing.T) {
	h := newHarness(t)
	h.dir = filepath.Join(t.TempDir(), "CANARY-path", ".codex")
	h.src = newSource(h.dir)
	write(t, h.path("2026/11/04/rollout-old.jsonl"), fixture(t, "home/sessions/"+fixtureDay4))
	h.collect()
	var b bytes.Buffer
	b.WriteString(`{"timestamp":"2026-11-04T12:00:10Z","type":"session_meta","payload":{"cwd":"/root/CANARY-CONTENT","instructions":"CANARY-CONTENT"}}` + "\n")
	b.Write(turnContext(tNow.Add(15*time.Second), "CANARY-CONTENT model with spaces"))
	b.WriteString(`{"timestamp":"2026-11-04T12:00:20Z","type":"event_msg","payload":{"type":"user_message","message":"CANARY-CONTENT prompt"}}` + "\n")
	b.WriteString(`{"type":"event_msg","payload":{"type":"token_count","CANARY-CONTENT` + "\n")
	b.Write(tokenLine(tNow.Add(30*time.Second), tu{2, 0, 0, 1, 0}, tu{2, 0, 0, 1, 0},
		limits("codex", "5,300,1793801100", "", `{"has_credits":true,"unlimited":false,"balance":"CANARY-CONTENT"}`)))
	write(t, h.path("2026/11/04/rollout-c.jsonl"), b.Bytes())
	h.now = tNow.Add(time.Minute)
	got := h.collect() // checks samples and state for the canary
	if got["agent_requests_total{model=other,outcome=ok}"] != 1 || h.parse != 1 {
		t.Fatalf("got %v parse=%d", got, h.parse)
	}
	if _, ok := got["agent_quota_credits_balance{scope=all}"]; ok {
		t.Fatalf("unparseable balance exported: %v", got)
	}
	if err := os.RemoveAll(h.path("")); err != nil {
		t.Fatal(err)
	}
	res, _, err := h.src.Collect(context.Background(), h.now, h.st)
	if err == nil {
		t.Fatal("missing sessions must fail")
	}
	noCanary(t, res.Samples, err)
}

// The model cap folds models beyond max_models_per_profile into "other", decided when a call is counted.
func TestModelCapFolds(t *testing.T) {
	h := newHarness(t)
	h.src.MaxModels = 1
	h.collect()
	var b bytes.Buffer
	b.Write(turnContext(tNow.Add(10*time.Second), "model-one"))
	b.Write(tokenLine(tNow.Add(20*time.Second), tu{1, 0, 0, 0, 0}, tu{1, 0, 0, 0, 0}, "null"))
	b.Write(turnContext(tNow.Add(30*time.Second), "model-two"))
	b.Write(tokenLine(tNow.Add(40*time.Second), tu{2, 0, 0, 0, 0}, tu{3, 0, 0, 0, 0}, "null"))
	write(t, h.path("2026/11/04/rollout-m.jsonl"), b.Bytes())
	h.now = tNow.Add(time.Minute)
	equal(t, "cap", counters(h.collect()), merge(tokens("model-one", 1, 0, 0, 0, 0, 1), tokens("other", 2, 0, 0, 0, 0, 1)))
}

func wantReason(t *testing.T, err error, reason string) {
	t.Helper()
	var se *core.SourceError
	if !errors.As(err, &se) || se.Reason != reason {
		t.Fatalf("want %s, got %v", reason, err)
	}
}

// Missing or unreadable state is an error, never zero values.
func TestMissingSessionsIsAnError(t *testing.T) {
	h := newHarness(t)
	h.copyTree()
	h.collect()
	if err := os.RemoveAll(h.path("")); err != nil {
		t.Fatal(err)
	}
	res, _, err := h.src.Collect(context.Background(), h.now, h.st)
	wantReason(t, err, core.ReasonNotFound)
	if len(res.Samples) != 0 {
		t.Fatalf("samples with missing sessions: %v", res.Samples)
	}
	h.restart()
	h.dir = filepath.Join(h.dir, "nope")
	h.src = newSource(h.dir)
	_, _, err = h.src.Collect(context.Background(), h.now, nil)
	wantReason(t, err, core.ReasonNotFound)
}

func TestSymlinkedSessionsRefused(t *testing.T) {
	h := newHarness(t)
	other := t.TempDir()
	write(t, filepath.Join(other, fixtureDay4), fixture(t, "home/sessions/"+fixtureDay4))
	if err := os.Remove(h.path("")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, h.path("")); err != nil {
		t.Fatal(err)
	}
	res, _, err := h.src.Collect(context.Background(), h.now, nil)
	wantReason(t, err, core.ReasonPermission)
	if len(res.Samples) != 0 {
		t.Fatalf("samples through a link: %v", res.Samples)
	}
}

// A linked rollout file or day directory inside sessions/ is never followed.
func TestSymlinkedRolloutRefused(t *testing.T) {
	h := newHarness(t)
	h.collect()
	other := t.TempDir()
	write(t, filepath.Join(other, "rollout-x.jsonl"), fixture(t, "home/sessions/"+fixtureDay4))
	if err := os.MkdirAll(h.path("2026/11/04"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "rollout-x.jsonl"), h.path("2026/11/04/rollout-x.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, h.path("2026/11/05")); err != nil {
		t.Fatal(err)
	}
	h.now = tNow.Add(time.Minute)
	if got := h.collect(); len(got) != 0 {
		t.Fatalf("read through a link: %v", got)
	}
}

func TestBuild(t *testing.T) {
	cfg := config.Default()
	srcs := Build(cfg, config.Resolved{Harness: config.HarnessCodex, Alias: "cx", Dir: "/x"}, "node-a")
	if len(srcs) != 1 {
		t.Fatalf("sources: %d", len(srcs))
	}
	s := srcs[0].(*RolloutsSource)
	if s.Name() != "codex-rollouts" || s.Harness() != "codex" || s.Profile() != "cx" || s.Network() ||
		s.MinInterval() != time.Minute || s.Account != "cx" || s.StaleAfter != 45*time.Minute || s.MaxModels != 20 {
		t.Fatalf("defaults: %+v", s)
	}
	cfg.Sources = map[string]config.Source{"codex-rollouts": {MinInterval: config.Duration(5 * time.Minute)}}
	s = Build(cfg, config.Resolved{Alias: "cx", Account: "acct-a1", Dir: "/x"}, "node-a")[0].(*RolloutsSource)
	if s.MinInterval() != 5*time.Minute || s.Account != "acct-a1" {
		t.Fatalf("overrides: %+v", s)
	}
}

// A collect cut short (read timeout or shutdown) must keep the state of the files it did not reach. Before the fix
// their offsets were pruned, so the next collect re-read them from 0 and counted the last hour twice: here turn A,
// counted a minute before the cut-short collect, would be counted again.
func TestCutShortCollectKeepsUnreadFiles(t *testing.T) {
	h := newHarness(t)
	h.copyTree()
	h.collect() // baseline: history not counted
	appendFile(t, h.path(fixtureDay4), tokenLine(tNow.Add(time.Minute), tu{100, 40, 0, 20, 5}, tu{2160, 1540, 200, 335, 125}, "null"))
	h.now = tNow.Add(2 * time.Minute)
	afterA := tokens("gpt-5-codex", 60, 15, 40, 0, 5, 1)
	equal(t, "turn A", counters(h.collect()), afterA)

	appendFile(t, h.path(fixtureDay4), tokenLine(tNow.Add(3*time.Minute), tu{10, 0, 0, 1, 0}, tu{2170, 1540, 200, 336, 125}, "null"))
	h.now = tNow.Add(4 * time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, st, err := h.src.Collect(ctx, h.now, h.st)
	if err != nil {
		t.Fatal(err)
	}
	h.st = st
	h.restart() // the next collect sees only what was persisted
	h.now = tNow.Add(5 * time.Minute)
	equal(t, "turn A once, then turn B", counters(h.collect()), tokens("gpt-5-codex", 70, 16, 40, 0, 5, 2))
}

// A file read from an offset inside it (present at the first collect) counts its next call under the model of its
// last turn_context before that offset, even when that line is outside the tail searched for quota and an older
// turn_context with another model lies further back.
func TestModelFoundBeforeStartOffset(t *testing.T) {
	h := newHarness(t)
	filler := func(b *bytes.Buffer, n int) {
		line := `{"timestamp":"2026-11-04T08:00:00Z","type":"response_item","payload":{"type":"message","content":"CANARY-CONTENT ` +
			strings.Repeat("x", 4000) + `"}}` + "\n"
		for b.Len() < n {
			b.WriteString(line)
		}
	}
	var b bytes.Buffer
	b.Write(turnContext(tStart, "gpt-old"))
	filler(&b, 3<<20)
	b.Write(turnContext(tStart.Add(time.Minute), "gpt-5-codex"))
	filler(&b, b.Len()+scanTail+(scanTail/2))
	b.Write(tokenLine(tStart.Add(2*time.Minute), tu{5, 0, 0, 1, 0}, tu{5, 0, 0, 1, 0}, "null"))
	write(t, h.path("2026/11/04/rollout-long.jsonl"), b.Bytes())
	write(t, h.path("2026/11/04/rollout-none.jsonl"), tokenLine(tStart, tu{1, 0, 0, 0, 0}, tu{1, 0, 0, 0, 0}, "null"))
	if got := h.collect(); len(counters(got)) != 0 {
		t.Fatalf("history replayed: %v", counters(got))
	}

	appendFile(t, h.path("2026/11/04/rollout-long.jsonl"), tokenLine(tNow.Add(time.Minute), tu{10, 0, 0, 1, 0}, tu{15, 0, 0, 2, 0}, "null"))
	appendFile(t, h.path("2026/11/04/rollout-none.jsonl"), tokenLine(tNow.Add(time.Minute), tu{7, 0, 0, 0, 0}, tu{8, 0, 0, 0, 0}, "null"))
	h.now = tNow.Add(2 * time.Minute)
	want := merge(tokens("gpt-5-codex", 10, 1, 0, 0, 0, 1), tokens("unknown", 7, 0, 0, 0, 0, 1))
	equal(t, "model before the start offset", counters(h.collect()), want)
	if h.parse != 0 {
		t.Fatalf("parse errors: %d", h.parse)
	}
}

// A read that fails after the file was listed (too many open files, an NFS I/O error) leaves the file without a
// model and counts nothing, so the search runs again next time: a passing failure must not put calls under
// "unknown". Either every open fails, or only the search's while the count read would work.
func TestModelSearchRetriedAfterReadFailure(t *testing.T) {
	orig := openRollout
	defer func() { openRollout = orig }()
	for name, failures := range map[string]int{"every read fails": 1 << 30, "only the search fails": 1} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			var b bytes.Buffer
			b.Write(turnContext(tStart, "gpt-5-codex"))
			for b.Len() < scanTail+scanTail/2 {
				b.WriteString(`{"timestamp":"2026-11-04T08:00:00Z","type":"response_item","payload":{"content":"CANARY-CONTENT ` +
					strings.Repeat("x", 4000) + `"}}` + "\n")
			}
			p := h.path("2026/11/04/rollout-flaky.jsonl")
			write(t, p, b.Bytes())
			h.collect()
			appendFile(t, p, tokenLine(tNow.Add(time.Minute), tu{10, 0, 0, 1, 0}, tu{10, 0, 0, 1, 0}, "null"))

			left := failures
			openRollout = func(path string) (*os.File, os.FileInfo, error) {
				if left > 0 {
					left--
					return nil, nil, syscall.EMFILE
				}
				return orig(path)
			}
			h.now = tNow.Add(2 * time.Minute)
			if got := counters(h.collect()); len(got) != 0 {
				t.Fatalf("counted without a model: %v", got)
			}
			openRollout = orig
			h.now = tNow.Add(3 * time.Minute)
			equal(t, "after the read works again", counters(h.collect()), tokens("gpt-5-codex", 10, 1, 0, 0, 0, 1))
		})
	}
}
