package zai

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "zai", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustParse(t *testing.T, body []byte) parsed {
	t.Helper()
	p, reason := parseQuota(body)
	if reason != "" {
		t.Fatalf("parse reason %s", reason)
	}
	return p
}

func find(t *testing.T, r *reading, win, scope string) window {
	t.Helper()
	for _, w := range r.Windows {
		if w.Window == win && w.Scope == scope {
			return w
		}
	}
	t.Fatalf("no window %s/%s in %+v", win, scope, r.Windows)
	return window{}
}

func TestParseGlobalFixture(t *testing.T) {
	p := mustParse(t, fixtureBytes(t, "quota-limit-global.json"))
	if p.Unknown != 0 || p.Reading.Level != "lite" || len(p.Reading.Windows) != 3 {
		t.Fatalf("%+v", p)
	}
	h5 := find(t, p.Reading, "5h", "all")
	d7 := find(t, p.Reading, "7d", "all")
	mo := find(t, p.Reading, "monthly", "tool_calls")
	for _, w := range []window{h5, d7, mo} {
		if w.Ratio == nil || *w.Ratio != 0 {
			t.Fatalf("%s ratio %v", w.Window, w.Ratio)
		}
	}
	if h5.Reset != 0 {
		t.Fatalf("5h window not started must have no reset, got %d", h5.Reset)
	}
	if d7.Reset != 1767830400 || mo.Reset != 1769904000 {
		t.Fatalf("resets %d %d (ms must become seconds)", d7.Reset, mo.Reset)
	}
	if mo.Used == nil || *mo.Used != 0 || mo.Limit == nil || *mo.Limit != 100 {
		t.Fatalf("calls %v %v", mo.Used, mo.Limit)
	}
	if h5.Used != nil || h5.Limit != nil || d7.Used != nil {
		t.Fatal("call counts only belong to the TIME_LIMIT window")
	}
}

func TestParseUsedFixture(t *testing.T) {
	p := mustParse(t, fixtureBytes(t, "quota-limit-used.json"))
	if p.Reading.Level != "pro" {
		t.Fatalf("level %q", p.Reading.Level)
	}
	if r := find(t, p.Reading, "5h", "all").Ratio; r == nil || *r != 0.37 {
		t.Fatalf("5h %v", r)
	}
	if r := find(t, p.Reading, "7d", "all").Ratio; r == nil || *r != 0.625 {
		t.Fatalf("7d %v", r)
	}
	mo := find(t, p.Reading, "monthly", "tool_calls")
	if mo.Ratio == nil || math.Abs(*mo.Ratio-0.042) > 1e-12 {
		t.Fatalf("monthly derives currentValue/usage, got %v", mo.Ratio)
	}
	if *mo.Used != 42 || *mo.Limit != 1000 {
		t.Fatal("calls")
	}
}

func TestParseUnknownFixture(t *testing.T) {
	p := mustParse(t, fixtureBytes(t, "quota-limit-unknown.json"))
	if p.Unknown != 4 {
		t.Fatalf("unknown count %d, want 4 (new type, unit 9, number 0, monthly number 2)", p.Unknown)
	}
	if len(p.Reading.Windows) != 1 {
		t.Fatalf("%+v", p.Reading.Windows)
	}
	w := find(t, p.Reading, "12h", "all")
	if w.Ratio == nil || *w.Ratio != 0.25 {
		t.Fatalf("numeric string percentage: %v", w.Ratio)
	}
	if p.Reading.Level != "unknown" {
		t.Fatalf("level %q", p.Reading.Level)
	}
}

func TestParseNoLimits(t *testing.T) {
	for _, body := range []string{string(fixtureBytes(t, "quota-limit-nolimits.json")),
		`{"success":true,"data":{"level":"lite","limits":[]}}`, `{"success":true,"data":{"limits":null}}`, `{"success":true}`} {
		p, reason := parseQuota([]byte(body))
		if reason != core.ReasonParse || p.Reading != nil {
			t.Fatalf("%s: reason %q reading %v", body, reason, p.Reading)
		}
	}
}

func TestParseEnvelopeFailures(t *testing.T) {
	cases := map[int]string{
		1000: core.ReasonAuthExpired, 1001: core.ReasonAuthExpired, 1002: core.ReasonAuthExpired, 1003: core.ReasonAuthExpired, 1004: core.ReasonAuthExpired,
		1302: core.ReasonRateLimited, 1305: core.ReasonRateLimited,
		500: core.ReasonUpstream, 1100: core.ReasonUpstream, 0: core.ReasonUpstream,
	}
	for code, want := range cases {
		body := `{"code":` + strconv.Itoa(code) + `,"msg":"x","success":false}`
		if _, reason := parseQuota([]byte(body)); reason != want {
			t.Errorf("code %d: %s, want %s", code, reason, want)
		}
	}
	if _, reason := parseQuota(fixtureBytes(t, "quota-limit-authfail.json")); reason != core.ReasonAuthExpired {
		t.Fatalf("authfail fixture: %s", reason)
	}
	// success:false without a code, or with a code that is not a number, is upstream, never data.
	for _, body := range []string{`{"success":false}`, `{"success":false,"code":"x"}`} {
		if _, reason := parseQuota([]byte(body)); reason != core.ReasonUpstream {
			t.Errorf("%s: %s", body, reason)
		}
	}
	// HTTP 200 alone never means data: not an envelope, not JSON, wrong shapes.
	for _, body := range []string{``, `not json`, `[]`, `{}`, `{"data":{"limits":[]}}`, `{"success":"yes"}`, `{"success":true,"data":[]}`} {
		if p, reason := parseQuota([]byte(body)); reason != core.ReasonParse || p.Reading != nil {
			t.Errorf("%q: %q", body, reason)
		}
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]string{"lite": "lite", "LITE": "lite", "Pro": "pro", "max": "max", "team": "team", "": "unknown", "enterprise": "unknown", "pro ": "unknown"} {
		body := `{"success":true,"data":{"level":"` + in + `","limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":1}]}}`
		if got := mustParse(t, []byte(body)).Reading.Level; got != want {
			t.Errorf("level %q -> %q, want %q", in, got, want)
		}
	}
	for _, body := range []string{
		`{"success":true,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":1}]}}`,
		`{"success":true,"data":{"level":7,"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":1}]}}`,
	} {
		if got := mustParse(t, []byte(body)).Reading.Level; got != "unknown" {
			t.Errorf("%s -> %q", body, got)
		}
	}
}

func TestParseMergeDuplicates(t *testing.T) {
	body := `{"success":true,"data":{"level":"lite","limits":[
		{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":10,"nextResetTime":1767243600000},
		{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":40,"nextResetTime":1767236400000},
		{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":20}]}}`
	p := mustParse(t, []byte(body))
	if len(p.Reading.Windows) != 1 {
		t.Fatalf("%+v", p.Reading.Windows)
	}
	w := p.Reading.Windows[0]
	if *w.Ratio != 0.4 || w.Reset != 1767236400 {
		t.Fatalf("max ratio, earliest reset expected: %v %d", *w.Ratio, w.Reset)
	}
}

func TestParseImplausibleValues(t *testing.T) {
	lim := func(extra string) string {
		return `{"success":true,"data":{"level":"lite","limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5` + extra + `}]}}`
	}
	for name, tc := range map[string]struct {
		extra      string
		ratio      bool
		reset      bool
		wantRatioV float64
	}{
		"reset zero":       {`,"percentage":1,"nextResetTime":0`, true, false, 0.01},
		"reset negative":   {`,"percentage":1,"nextResetTime":-5`, true, false, 0.01},
		"reset year 2200":  {`,"percentage":1,"nextResetTime":7258118400000`, true, false, 0.01},
		"reset huge":       {`,"percentage":1,"nextResetTime":1e30`, true, false, 0.01},
		"reset string bad": {`,"percentage":1,"nextResetTime":"soon"`, true, false, 0.01},
		"percent negative": {`,"percentage":-1,"nextResetTime":1767243600000`, false, true, 0},
		"percent string":   {`,"percentage":"nope"`, false, false, 0},
		"percent absent":   {``, false, false, 0},
		"percent null":     {`,"percentage":null`, false, false, 0},
	} {
		t.Run(name, func(t *testing.T) {
			p := mustParse(t, []byte(lim(tc.extra)))
			w := p.Reading.Windows[0]
			if (w.Ratio != nil) != tc.ratio || (w.Reset != 0) != tc.reset {
				t.Fatalf("ratio %v reset %d", w.Ratio, w.Reset)
			}
			if tc.ratio && math.Abs(*w.Ratio-tc.wantRatioV) > 1e-12 {
				t.Fatalf("ratio %v", *w.Ratio)
			}
		})
	}
	// usage 0 must give no ratio, never NaN or Inf
	p := mustParse(t, []byte(`{"success":true,"data":{"level":"lite","limits":[{"type":"TIME_LIMIT","unit":5,"number":1,"usage":0,"currentValue":0}]}}`))
	if w := p.Reading.Windows[0]; w.Ratio != nil {
		t.Fatalf("ratio %v for usage 0", *w.Ratio)
	}
}

func TestParseWindowMapping(t *testing.T) {
	cases := map[string]string{ // limit JSON -> window/scope, "" = dropped
		`{"type":"TOKENS_LIMIT","unit":3,"number":5}`:          "5h/all",
		`{"type":"TOKENS_LIMIT","unit":3,"number":24}`:         "24h/all",
		`{"type":"TOKENS_LIMIT","unit":6,"number":1}`:          "7d/all",
		`{"type":"TOKENS_LIMIT","unit":6,"number":2}`:          "14d/all",
		`{"type":"TIME_LIMIT","unit":5,"number":1}`:            "monthly/tool_calls",
		`{"type":"TIME_LIMIT","unit":3,"number":5}`:            "",
		`{"type":"TOKENS_LIMIT","unit":5,"number":1}`:          "",
		`{"type":"TOKENS_LIMIT","unit":3,"number":1.5}`:        "",
		`{"type":"TOKENS_LIMIT","unit":3,"number":-1}`:         "",
		`{"type":"TOKENS_LIMIT","unit":6,"number":9999999999}`: "",
		`{"type":"TOKENS_LIMIT","unit":3}`:                     "",
		`{"unit":3,"number":5}`:                                "",
		`"scalar"`:                                             "",
	}
	for lim, want := range cases {
		body := `{"success":true,"data":{"level":"lite","limits":[` + lim + `,{"type":"TOKENS_LIMIT","unit":3,"number":7}]}}`
		p := mustParse(t, []byte(body))
		got := map[string]bool{}
		for _, w := range p.Reading.Windows {
			got[w.Window+"/"+w.Scope] = true
		}
		wantSet := map[string]bool{"7h/all": true}
		if want != "" {
			wantSet[want] = true
		}
		if len(got) != len(wantSet) {
			t.Errorf("%s: got %v, want %v", lim, got, wantSet)
			continue
		}
		for k := range wantSet {
			if !got[k] {
				t.Errorf("%s: got %v, want %v", lim, got, wantSet)
			}
		}
	}
}

var windowLabelRe = regexp.MustCompile(`^[a-z0-9_-]{1,16}$`)

func TestParsedLabelsAreBounded(t *testing.T) {
	for _, f := range []string{"quota-limit-global.json", "quota-limit-used.json", "quota-limit-unknown.json"} {
		p := mustParse(t, fixtureBytes(t, f))
		for _, w := range p.Reading.Windows {
			if !windowLabelRe.MatchString(w.Window) || !windowLabelRe.MatchString(w.Scope) {
				t.Fatalf("%s: label %q/%q", f, w.Window, w.Scope)
			}
		}
	}
}
