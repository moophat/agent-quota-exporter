package core

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ContractVersion is exposed in agent_quota_exporter_build_info{contract}.
const ContractVersion = "0"

type family struct{ name, typ, help string }

// families: every name the exporter may emit, in output order, with HELP text taken from
// specs/metric-contract.md. A sample whose name is not here is a programming error and is dropped.
var families = []family{
	{"agent_lane_busy", "gauge", "1 if any lane slot is processing, 0 if all are idle."},
	{"agent_lane_queued", "gauge", "Requests queued on the lane."},
	{"agent_lane_slots", "gauge", "Slots reported by the lane."},
	{"agent_lane_requests_total", "counter", "Requests reported by the lane since server start."},
	{"agent_lane_info", "gauge", "Lane model and engine capabilities."},
	{"agent_event_store_write_success", "gauge", "1 if the last durable event write succeeded, 0 otherwise."},
	{"agent_collector_backlog_bytes", "gauge", "Unread source bytes, including unvisited files."},
	{"agent_event_store_size_bytes", "gauge", "Total local event-store footprint in bytes."},
	{"agent_event_store_available_bytes", "gauge", "Available bytes on the event-store filesystem."},
	{"agent_event_store_pressure", "gauge", "1 when event-store capacity is under pressure."},
	{"agent_quota_utilization_ratio", "gauge", "Share of a subscription quota window consumed, as reported by the vendor."},
	{"agent_quota_reset_timestamp_seconds", "gauge", "When the quota window resets, Unix seconds."},
	{"agent_quota_limit_info", "gauge", "Vendor-reported limit state, one series per active limit entry."},
	{"agent_quota_observed_timestamp_seconds", "gauge", "When the vendor was last successfully asked, Unix seconds."},
	{"agent_quota_credits_available", "gauge", "1 if the vendor reports credits left or unlimited, 0 if out."},
	{"agent_quota_credits_balance", "gauge", "Credit balance as reported, in the vendor's own credit unit."},
	{"agent_quota_used_calls", "gauge", "Calls used in a call-counted quota window, as reported by the vendor."},
	{"agent_quota_limit_calls", "gauge", "Calls allowed in a call-counted quota window, as reported by the vendor."},
	{"agent_quota_reset_credits_available", "gauge", "Reset slots the vendor says can be used now."},
	{"agent_quota_reset_credits_next_expiry_timestamp_seconds", "gauge", "When the soonest-expiring unused reset slot expires, Unix seconds."},
	{"agent_tokens_total", "counter", "Tokens consumed, read from local agent state."},
	{"agent_requests_total", "counter", "Model calls, read from local agent state."},
	{"agent_request_errors_total", "counter", "Model calls that failed, by bounded class."},
	{"agent_cost_usd_total", "counter", "Cost as reported by the harness, USD. Not a bill."},
	{"agent_account_logged_in", "gauge", "1 if the profile holds a usable login (credential present and not known-expired)."},
	{"agent_account_info", "gauge", "Slow-changing account facts."},
	{"agent_credential_expiry_timestamp_seconds", "gauge", "Access-token expiry time, Unix seconds (never the token)."},
	{"agent_sessions", "gauge", "Live sessions by state."},
	{"agent_router_connection_enabled", "gauge", "1 if the operator enabled the router connection, 0 if disabled."},
	{"agent_router_connection_up", "gauge", "1 if the router connection is active, its last test passed and it is not rate-limited."},
	{"agent_router_rate_limited_until_timestamp_seconds", "gauge", "When the router's own rate limit on the connection ends, Unix seconds."},
	{"agent_source_up", "gauge", "1 if the last read of the source succeeded or a fresh cache entry exists; fresh age <= max(stale_after, 2*poll_interval + read_timeout + 15s)."},
	{"agent_source_last_success_timestamp_seconds", "gauge", "Last successful read of the source, Unix seconds."},
	{"agent_source_cache_age_seconds", "gauge", "Age of the value being served, seconds."},
	{"agent_source_backoff_until_timestamp_seconds", "gauge", "Non-zero while a vendor 429 or error backoff is active, Unix seconds."},
	{"agent_source_read_seconds", "gauge", "Duration of the last read of the source, seconds."},
	{"agent_source_errors_total", "counter", "Read failures by bounded reason."},
	{"agent_quota_exporter_build_info", "gauge", "Build version and metric contract version."},
	{"agent_quota_exporter_profiles", "gauge", "Profiles known, by harness and origin."},
	{"agent_quota_exporter_state_epoch", "gauge", "When the current persisted state was initialized, Unix seconds; a change means counters restarted."},
	{"process_start_time_seconds", "gauge", "Start time of the process since unix epoch in seconds."},
}

// FamilyNames returns every family name the exporter may emit (used by the contract test).
func FamilyNames() []string {
	out := make([]string, len(families))
	for i, f := range families {
		out[i] = f.name
	}
	return out
}

// Exposition holds the process-level facts render needs besides the scheduler.
type Exposition struct {
	Host         string
	Static       []Label // config labels, appended to every series
	Version      string
	ProcessStart time.Time
	Profiles     map[[2]string]int // {harness, origin} → count
	Sched        *Scheduler
	Clock        Clock
	ExtraSamples func() []Sample
}

// Render writes the Prometheus text exposition (format 0.0.4).
func (e *Exposition) Render(w io.Writer) error {
	now := e.Clock.Now()
	by := map[string][]Sample{}
	// A duplicate label set would make the scrape invalid; plugins merge their own collisions. Between two
	// sources serving the same series (a live reading beside a passive one still cached from before) the sample
	// with the newer observed time wins, so a stale cache can never hide a fresher reading; ties keep the first.
	type slot struct {
		name string
		idx  int
		at   time.Time
	}
	pos := map[string]slot{}
	addAt := func(s Sample, at time.Time) {
		var k strings.Builder
		k.WriteString(s.Name)
		for _, l := range s.Labels {
			k.WriteString("\x00" + l.Name + "\x01" + l.Value)
		}
		if p, dup := pos[k.String()]; dup {
			if at.After(p.at) {
				by[p.name][p.idx] = s
				pos[k.String()] = slot{p.name, p.idx, at}
			}
			return
		}
		pos[k.String()] = slot{s.Name, len(by[s.Name]), at}
		by[s.Name] = append(by[s.Name], s)
	}
	add := func(s Sample) { addAt(s, time.Time{}) }
	if e.ExtraSamples != nil {
		for _, s := range e.ExtraSamples() {
			add(s)
		}
	}

	for _, sn := range e.Sched.snapshot(now) {
		for _, s := range sn.cache {
			addAt(s, sn.observedAt)
		}
		base := []Label{{"host", e.Host}, {"harness", sn.harness}, {"profile", sn.profile}, {"source", sn.name}}
		up := 0.0
		if (sn.attempted && sn.lastOK) || sn.fresh {
			up = 1
		}
		add(Sample{"agent_source_up", base, up})
		if !sn.lastSuccess.IsZero() {
			add(Sample{"agent_source_last_success_timestamp_seconds", base, float64(sn.lastSuccess.Unix())})
		}
		if sn.fresh {
			add(Sample{"agent_source_cache_age_seconds", base, math.Max(0, now.Sub(sn.observedAt).Seconds())})
		}
		bu := 0.0
		if sn.backoffUntil.After(now) {
			bu = float64(sn.backoffUntil.Unix())
		}
		add(Sample{"agent_source_backoff_until_timestamp_seconds", base, bu})
		if sn.attempted {
			add(Sample{"agent_source_read_seconds", base, sn.readSeconds})
		}
		for r, v := range sn.errors {
			add(Sample{"agent_source_errors_total", append(append([]Label{}, base...), Label{"reason", r}), v})
		}
	}
	add(Sample{"agent_quota_exporter_build_info", []Label{{"version", e.Version}, {"contract", ContractVersion}}, 1})
	for k, n := range e.Profiles {
		add(Sample{"agent_quota_exporter_profiles", []Label{{"harness", k[0]}, {"origin", k[1]}}, float64(n)})
	}
	add(Sample{"agent_quota_exporter_state_epoch", nil, float64(e.Sched.state.Epoch())})
	add(Sample{"process_start_time_seconds", nil, float64(e.ProcessStart.Unix())})

	var b strings.Builder
	for _, f := range families {
		ss := by[f.name]
		if len(ss) == 0 {
			continue
		}
		lines := make([]string, 0, len(ss))
		seen := map[string]bool{}
		for _, s := range ss {
			l := f.name + formatLabels(append(append([]Label{}, s.Labels...), e.Static...))
			if seen[l] {
				continue // duplicate series are invalid exposition; first wins
			}
			seen[l] = true
			lines = append(lines, l+" "+formatValue(s.Value))
		}
		sort.Strings(lines)
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.typ)
		for _, l := range lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func formatLabels(ls []Label) string {
	if len(ls) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, l := range ls {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.Name)
		b.WriteString(`="`)
		b.WriteString(escape(l.Value))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

var escaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escape(s string) string { return escaper.Replace(s) }

func formatValue(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
