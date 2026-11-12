package config

import (
	"strings"
	"testing"
)

func TestPerSessionAndJobsAccepted(t *testing.T) {
	body := "version: 1\ndiscovery: {mode: \"off\"}\nmetrics: {per_session: true, max_sessions_per_profile: 20}\n" +
		"jobs: {lock_dir: /run/runner, call_log: /var/log/runner/calls.jsonl}\n" +
		"sources:\n  job-locks: {min_interval: 10s}\n  opencode-http: {min_interval: 10s}\n" +
		"profiles:\n- {harness: opencode, alias: a, dir: /x, url: \"http://127.0.0.1:4096\"}\n"
	c, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Metrics.PerSession || c.Metrics.MaxSessionsPerProfile != 20 || c.Jobs.LockDir != "/run/runner" {
		t.Fatalf("not read: %+v %+v", c.Metrics, c.Jobs)
	}
	profiles, _ := c.Resolve(func(string) string { return "" })
	if len(profiles) != 1 || profiles[0].URL != "http://127.0.0.1:4096" {
		t.Fatalf("url not carried to the resolved profile: %+v", profiles)
	}
	d, err := Load("")
	if err != nil || d.Metrics.MaxSessionsPerProfile != 50 || d.Metrics.PerSession {
		t.Fatalf("defaults %+v %v", d.Metrics, err)
	}
}

func TestSessionAndJobConfigFailures(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"cap zero":         {"version: 1\nmetrics: {max_sessions_per_profile: 0}\n", "max_sessions_per_profile"},
		"cap too big":      {"version: 1\nmetrics: {max_sessions_per_profile: 501}\n", "max_sessions_per_profile"},
		"relative lock":    {"version: 1\njobs: {lock_dir: run/x}\n", "jobs.lock_dir"},
		"relative log":     {"version: 1\njobs: {call_log: x.jsonl}\n", "jobs.call_log"},
		"remote url":       {"version: 1\nprofiles:\n- {harness: opencode, alias: a, dir: /x, url: \"http://10.0.0.5:4096\"}\n", "loopback"},
		"url credentials":  {"version: 1\nprofiles:\n- {harness: opencode, alias: a, dir: /x, url: \"http://u:p@127.0.0.1:4096\"}\n", "without credentials"},
		"url scheme":       {"version: 1\nprofiles:\n- {harness: opencode, alias: a, dir: /x, url: \"ftp://127.0.0.1\"}\n", "HTTP(S)"},
		"allow_remote now": {"version: 1\nprofiles:\n- {harness: codex, alias: a, dir: /x, allow_remote: true}\n", "allow_remote"},
		"label session":    {"version: 1\nlabels: {session: x}\n", "collides"},
		"label entrypoint": {"version: 1\nlabels: {entrypoint: x}\n", "collides"},
		"label tmux":       {"version: 1\nlabels: {tmux_session: x}\n", "collides"},
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

func TestRemoteOpencodeURLNeedsAllowRemote(t *testing.T) {
	body := "version: 1\nprofiles:\n- {harness: opencode, alias: a, dir: /x, url: \"http://10.0.0.5:4096\", allow_remote: true}\n"
	if _, err := Load(writeCfg(t, body)); err != nil {
		t.Fatal(err)
	}
}
