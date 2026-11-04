// Package agy runs the agy client's own /usage command (`agy-usage`, opt-in) and parses its quota buckets.
//
// The exporter never calls the vendor and never touches a token: it spawns the vendor's official client with a
// fixed argv, the way a person would type it, and reads the JSON it prints. The client may refresh its own login
// while doing so, as any agy run does, so it always runs as the owner of the profile HOME (specs/sources.md, "agy").
package agy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
)

const (
	// Harness and Provider are the contract label values for this plugin.
	Harness  = "agy"
	Provider = "google"
	// SourceName is the contract `source` value.
	SourceName = "agy-usage"

	defaultInterval = 30 * time.Minute
	defaultBackoff  = 60 * time.Minute // after a non-zero exit or a timeout (config.Floors["agy-usage"])
	// GuardBackoff pauses the source after output that shows the client spent a model turn or answered another
	// command: the exporter must never cost tokens, so it stops asking for a day rather than retry.
	GuardBackoff = 24 * time.Hour

	defaultTimeout = 30 * time.Second // one client run
	defaultGrace   = 5 * time.Second  // after SIGTERM, before SIGKILL: lets the client finish writing its token file
	maxStdout      = 256 << 10
	fallbackPath   = "/usr/local/bin:/usr/bin:/bin"
)

// argv is fixed: no part of it comes from config, the profile or the environment.
var argv = []string{"-p", "/usage", "--output-format", "json"}

// runSlot serialises agy runs host-wide (within this process): two clients refreshing logins at once is load
// the vendor did not ask for, and one at a time keeps the exporter's footprint flat.
var runSlot = make(chan struct{}, 1)

// Build returns the sources for one resolved agy profile: `agy-usage` when the profile opts in, else none.
func Build(cfg *config.Config, r config.Resolved, host string) []core.Source {
	if !r.Quota["agy_usage"] {
		return nil
	}
	return []core.Source{&UsageSource{Host: host, Alias: r.Alias, Dir: r.Dir, Account: r.Account,
		Interval: cfg.SourceInterval(SourceName, defaultInterval),
		Backoff:  cfg.SourceBackoff(SourceName, defaultBackoff)}}
}

// UsageSource is `agy-usage` for one profile (one agy HOME).
type UsageSource struct {
	Host, Alias string
	Dir         string // the HOME the client runs under (real path)
	// Account is the operator's alias for the login. Required: the same login can sit on many machines, and
	// the operator picks the one host that reads it by declaring the account there.
	Account  string
	Interval time.Duration
	Backoff  time.Duration

	// Bin is the absolute path of the agy client; empty means exec.LookPath("agy") at the first collect that
	// finds it. Timeout and Grace bound one run; zero means the defaults. Set directly only by tests.
	Bin     string
	Timeout time.Duration
	Grace   time.Duration
}

func (s *UsageSource) Name() string               { return SourceName }
func (s *UsageSource) Harness() string            { return Harness }
func (s *UsageSource) Profile() string            { return s.Alias }
func (s *UsageSource) MinInterval() time.Duration { return s.Interval }
func (s *UsageSource) Network() bool              { return true }

// NetGroup keeps agy's spacing and backoff apart from Claude's endpoint: a lock on one never pauses the other.
func (s *UsageSource) NetGroup() string { return Harness }

// ReadTimeout covers waiting for another profile's run and this run, each with its two grace periods (one for the
// client after the timeout, one for whatever of its process group outlives it).
func (s *UsageSource) ReadTimeout() time.Duration {
	return 2*(s.timeout()+2*s.grace()) + 5*time.Second
}

func (s *UsageSource) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return defaultTimeout
}

func (s *UsageSource) grace() time.Duration {
	if s.Grace > 0 {
		return s.Grace
	}
	return defaultGrace
}

type state struct {
	Last   []row  `json:"last,omitempty"`
	LastAt int64  `json:"last_at,omitempty"`
	Acct   string `json:"acct,omitempty"` // account label the last values belong to
}

// Restore serves the last good reading after a restart (staleness still applies in the core).
func (s *UsageSource) Restore(raw json.RawMessage) core.Result {
	var st state
	if json.Unmarshal(raw, &st) != nil || len(st.Last) == 0 || st.LastAt == 0 || s.Account == "" || st.Acct != s.Account {
		return core.Result{}
	}
	at := time.Unix(st.LastAt, 0)
	return core.Result{Samples: s.samples(st.Last, at), ObservedAt: at}
}

func (s *UsageSource) Collect(ctx context.Context, now time.Time, raw json.RawMessage) (core.Result, json.RawMessage, error) {
	if s.Account == "" {
		// A configuration error, not a read failure: nothing is spawned until the operator names the account.
		return core.Result{}, nil, core.Fail(core.ReasonOther)
	}
	var st state
	_ = json.Unmarshal(raw, &st)

	// fail serves the last good reading (the core drops it when stale) alongside the error, never another
	// account's reading under this profile.
	fail := func(err error) (core.Result, json.RawMessage, error) {
		if len(st.Last) > 0 && st.LastAt > 0 && st.Acct == s.Account {
			at := time.Unix(st.LastAt, 0)
			return core.Result{Samples: s.samples(st.Last, at), ObservedAt: at}, nil, err
		}
		return core.Result{}, nil, err
	}

	out, err := s.run(ctx)
	if err != nil {
		return fail(err)
	}
	rows, unknown, err := parse(out)
	if err != nil {
		return fail(err)
	}
	st = state{Last: rows, LastAt: now.Unix(), Acct: s.Account}
	blob, err := json.Marshal(st)
	if err != nil {
		return fail(core.Fail(core.ReasonOther))
	}
	res := core.Result{Samples: s.samples(rows, now), ObservedAt: now}
	if unknown > 0 {
		res.Errors = map[string]int{core.ReasonParse: unknown}
	}
	return res, blob, nil
}

// bin resolves the client once it is found; a client installed later is picked up at the next collect.
func (s *UsageSource) bin() (string, error) {
	if s.Bin == "" {
		p, err := exec.LookPath("agy")
		if err != nil {
			return "", core.Fail(core.ReasonNotFound)
		}
		s.Bin = p
	}
	if !filepath.IsAbs(s.Bin) {
		// a relative PATH entry would resolve against the profile's HOME, which the run uses as its cwd
		return "", core.Fail(core.ReasonNotFound)
	}
	return s.Bin, nil
}

// run spawns `agy -p /usage --output-format json` under the profile's HOME and returns its stdout.
func (s *UsageSource) run(ctx context.Context) ([]byte, error) {
	bin, err := s.bin()
	if err != nil {
		return nil, err
	}
	cred, err := s.credential()
	if err != nil {
		return nil, err
	}
	select {
	case runSlot <- struct{}{}:
	case <-ctx.Done():
		return nil, core.Fail(core.ReasonTimeout) // another profile's run held the slot; try next interval
	}
	defer func() { <-runSlot }()

	rctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	path := os.Getenv("PATH")
	if path == "" {
		path = fallbackPath // an empty PATH entry would mean the cwd
	}
	stdout := &capped{max: maxStdout}
	cmd := exec.CommandContext(rctx, bin, argv...)
	cmd.Env = []string{"HOME=" + s.Dir, "PATH=" + path, "LANG=C"}
	cmd.Dir = s.Dir
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = nil // discarded: it may carry anything, and is never logged
	// Own process group, so a wrapper script's children get the signals too; run as the HOME's owner (credential).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: cred}
	// On timeout: SIGTERM first, so the client can finish writing its token file; SIGKILL after the grace.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = s.grace()

	if err := cmd.Start(); err != nil {
		return nil, core.Fail(core.ErrReason(err))
	}
	werr := cmd.Wait()
	// Wait also returns when the client exited but a child of it still holds stdout (after WaitDelay), so the
	// group is cleared after every run, not only on timeout.
	s.reapGroup(cmd.Process.Pid)
	if rctx.Err() != nil {
		if ctx.Err() != nil {
			return nil, core.Fail(core.ReasonTimeout) // shutdown, not a hung client: no backoff to persist
		}
		return nil, &core.SourceError{Reason: core.ReasonTimeout, Backoff: s.backoff()}
	}
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			return nil, &core.SourceError{Reason: core.ReasonUpstream, Backoff: s.backoff()}
		}
		return nil, core.Fail(core.ReasonOther)
	}
	if stdout.over {
		return nil, core.Fail(core.ReasonParse)
	}
	return stdout.buf.Bytes(), nil
}

// credential checks the profile HOME and returns the identity the client runs as: always the HOME's owner, so a
// login refresh rewrites the token file under the owner's uid and never locks the owner out of their own agy. Nil
// means "as the exporter" (the exporter is the owner). The HOME must be a real directory, not a link.
func (s *UsageSource) credential() (*syscall.Credential, error) {
	fi, err := os.Lstat(s.Dir)
	if err != nil {
		return nil, core.Fail(core.ErrReason(err))
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.IsDir() {
		return nil, core.Fail(core.ReasonPermission) // a link or a file where the HOME should be: refuse to run
	}
	euid := uint32(os.Geteuid())
	uid, ok := runAs(euid, st.Uid)
	if !ok {
		return nil, core.Fail(core.ReasonPermission)
	}
	if uid == euid {
		return nil, nil
	}
	// Supplementary groups are dropped (Groups empty): the exporter's own (root's) groups must not reach the client,
	// and the owner's are not looked up. The gid is the HOME's group.
	return &syscall.Credential{Uid: uid, Gid: st.Gid, Groups: []uint32{}}, nil
}

// runAs decides which uid the client runs as for a HOME owned by owner, given the exporter's effective uid: always
// the owner. A root exporter switches to the owner (stays root for root's own HOME); any other exporter runs only
// its own HOME, since it cannot switch user.
func runAs(euid, owner uint32) (uint32, bool) {
	if euid != 0 && euid != owner {
		return 0, false
	}
	return owner, true
}

// reapGroup ends whatever of the client's process group is still running: SIGTERM, then the grace to finish
// writing a token file, then SIGKILL. An empty group (the usual case) costs one kill(2) returning ESRCH.
func (s *UsageSource) reapGroup(pgid int) {
	if syscall.Kill(-pgid, syscall.SIGTERM) == syscall.ESRCH {
		return
	}
	deadline := time.Now().Add(s.grace())
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if syscall.Kill(-pgid, 0) == syscall.ESRCH {
			return
		}
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

func (s *UsageSource) backoff() time.Duration {
	if s.Backoff > 0 {
		return s.Backoff
	}
	return defaultBackoff
}

func (s *UsageSource) samples(rows []row, at time.Time) []core.Sample {
	base := []core.Label{{Name: "host", Value: s.Host}, {Name: "harness", Value: Harness}, {Name: "profile", Value: s.Alias},
		{Name: "account", Value: s.Account}, {Name: "provider", Value: Provider}}
	with := func(extra ...core.Label) []core.Label { return append(append([]core.Label(nil), base...), extra...) }
	var out []core.Sample
	for _, r := range rows {
		ls := with(core.Label{Name: "window", Value: r.Window}, core.Label{Name: "scope", Value: r.Scope})
		out = append(out, core.Sample{Name: "agent_quota_utilization_ratio", Labels: ls, Value: r.Ratio})
		if r.Reset > 0 {
			out = append(out, core.Sample{Name: "agent_quota_reset_timestamp_seconds", Labels: ls, Value: float64(r.Reset)})
		}
	}
	return append(out, core.Sample{Name: "agent_quota_observed_timestamp_seconds", Labels: with(), Value: float64(at.Unix())})
}

// capped keeps the first max bytes and drops the rest, so a runaway client cannot grow the exporter's memory.
// It never returns an error: the client must be allowed to exit normally rather than die on a closed pipe.
type capped struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room < len(p) {
		c.over = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}
