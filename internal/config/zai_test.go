package config

import (
	"strings"
	"testing"
	"time"
)

func zaiProfile(extra string) string {
	return "version: 1\ndiscovery: {mode: off}\nprofiles:\n- {harness: zai, alias: z, account: zai-main, dir: /x, key_source: openclaw, quota: {zai_usage: true}" + extra + "}\n"
}

func TestZAIProfilesLoadAndResolve(t *testing.T) {
	for _, ks := range ZAIKeySources {
		body := "version: 1\ndiscovery: {mode: off}\nprofiles:\n- {harness: zai, alias: z, account: zai-main, dir: /x, key_source: " + ks + ", quota: {zai_usage: true}}\n"
		c, err := Load(writeCfg(t, body))
		if err != nil {
			t.Fatalf("%s: %v", ks, err)
		}
		rs, _ := c.Resolve(func(string) string { return "" })
		if len(rs) != 1 {
			t.Fatalf("%s: %d resolved", ks, len(rs))
		}
		r := rs[0]
		if r.Harness != "zai" || r.KeySource != ks || r.Account != "zai-main" || r.Origin != "config" || !r.Quota.Enabled("zai_usage") {
			t.Fatalf("%s: bad resolved %+v", ks, r)
		}
	}
}

func TestZAIValidationFailures(t *testing.T) {
	p := "version: 1\nprofiles:\n- "
	cases := map[string]struct{ body, want string }{
		"missing key_source":   {p + "{harness: zai, alias: z, account: a, dir: /x, quota: {zai_usage: true}}\n", "profiles[0].key_source: required"},
		"zcode":                {p + "{harness: zai, alias: z, account: a, dir: /x, key_source: zcode, quota: {zai_usage: true}}\n", "unsupported"},
		"unknown key_source":   {p + "{harness: zai, alias: z, account: a, dir: /x, key_source: nope, quota: {zai_usage: true}}\n", "profiles[0].key_source: \"nope\" is not one of"},
		"missing account":      {p + "{harness: zai, alias: z, dir: /x, key_source: openclaw, quota: {zai_usage: true}}\n", "profiles[0].account: required"},
		"missing dir":          {p + "{harness: zai, alias: z, account: a, key_source: openclaw, quota: {zai_usage: true}}\n", "profiles[0].dir: required"},
		"region unknown key":   {p + "{harness: zai, alias: z, account: a, dir: /x, key_source: openclaw, region: cn, quota: {zai_usage: true}}\n", "region"},
		"absent zai_usage":     {p + "{harness: zai, alias: z, account: a, dir: /x, key_source: openclaw}\n", "profiles[0].quota.zai_usage: must be true"},
		"false zai_usage":      {p + "{harness: zai, alias: z, account: a, dir: /x, key_source: openclaw, quota: {zai_usage: false}}\n", "profiles[0].quota.zai_usage: must be true"},
		"zai_usage on codex":   {p + "{harness: codex, alias: z, dir: /x, quota: {zai_usage: true}}\n", "only for harness zai"},
		"key_source on claude": {p + "{harness: claude-code, alias: z, dir: /x, key_source: openclaw}\n", "profiles[0].key_source: only for harness zai"},
		"non-bool":             {p + "{harness: zai, alias: z, account: a, dir: /x, key_source: openclaw, quota: {zai_usage: 3}}\n", "must be a boolean"},
		"duplicate alias":      {p + "{harness: zai, alias: z, account: a, dir: /x, key_source: openclaw, quota: {zai_usage: true}}\n- {harness: zai, alias: z, account: b, dir: /y, key_source: opencode, quota: {zai_usage: true}}\n", "duplicate alias"},
		"api_key field":        {p + "{harness: zai, alias: z, account: a, dir: /x, key_source: openclaw, api_key: abc, quota: {zai_usage: true}}\n", "api_key"},
		"inline sk key":        {p + "{harness: zai, alias: z, account: a, dir: /x, key_source: openclaw, key: \"sk-abcdefabcdef\", quota: {zai_usage: true}}\n", "key"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeCfg(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestZAIDiscoveryAndDisable(t *testing.T) {
	for _, mode := range []string{"only", "add"} {
		c := Default()
		c.Discovery = Discovery{Mode: mode, Roots: nil}
		rs, _ := c.Resolve(func(string) string { return "" })
		for _, r := range rs {
			if r.Harness == "zai" {
				t.Fatalf("mode %s discovered a zai profile", mode)
			}
		}
	}
	c, err := Load(writeCfg(t, zaiProfile(", disable: true")))
	if err != nil {
		t.Fatal(err)
	}
	if rs, _ := c.Resolve(func(string) string { return "" }); len(rs) != 0 {
		t.Fatalf("disabled profile resolved: %+v", rs)
	}
}

func TestZAIFloors(t *testing.T) {
	f := Floors["zai-usage"]
	if f.MinInterval != 5*time.Minute || f.Backoff429 != 10*time.Minute {
		t.Fatalf("floor %+v", f)
	}
	if _, err := Load(writeCfg(t, "version: 1\nsources:\n  zai-usage: {min_interval: 1m}\n")); err == nil || !strings.Contains(err.Error(), "below the floor") {
		t.Fatalf("got %v", err)
	}
	c, err := Load(writeCfg(t, "version: 1\nsources:\n  zai-usage: {min_interval: 20m, backoff_429: 45m}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.SourceInterval("zai-usage", time.Minute) != 20*time.Minute || c.SourceBackoff("zai-usage", time.Minute) != 45*time.Minute {
		t.Fatal("overrides not honoured")
	}
}
