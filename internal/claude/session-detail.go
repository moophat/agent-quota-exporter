package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// defaultMaxSessions bounds the per-session series of one profile (metrics.max_sessions_per_profile).
const defaultMaxSessions = 50

// ProcAlive reports whether a pid exists on this host. Replaceable in tests.
var ProcAlive = func(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

// ProcStart returns field 22 of /proc/<pid>/stat (start time in clock ticks since boot), which the harness
// records as procStart. It parses after the last ")" because the command name may hold spaces and parentheses.
// Replaceable in tests.
var ProcStart = func(pid int) (string, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	return startFromStat(b)
}

func startFromStat(b []byte) (string, bool) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return "", false
	}
	f := strings.Fields(string(b[i+1:])) // f[0] is field 3
	if len(f) < 20 {
		return "", false
	}
	return f[19], true
}

// PidDomain returns this host's pid domain exactly as the harness writes it into a session file:
// "linux:<machine-id>:<pid namespace link>", e.g. linux:0123...:pid:[4026531836]. The namespace part alone is the
// same on every host, so the whole string is what tells hosts apart. "" when it cannot be built. Replaceable in tests.
var PidDomain = func() string {
	id, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return ""
	}
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return ""
	}
	return "linux:" + strings.TrimSpace(string(id)) + ":" + ns
}

// text, flag and num read a JSON value of the expected kind and give the zero value for any other kind, so one
// odd field never discards a whole session file.
type text string

func (t *text) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*t = text(s)
		return nil
	}
	var n json.Number
	if json.Unmarshal(b, &n) == nil {
		*t = text(n.String())
	}
	return nil
}

type flag bool

func (f *flag) UnmarshalJSON(b []byte) error {
	*f = string(bytes.TrimSpace(b)) == "true"
	return nil
}

type num float64

func (n *num) UnmarshalJSON(b []byte) error {
	var v float64
	if json.Unmarshal(b, &v) == nil {
		*n = num(v)
	}
	return nil
}

// sessionDoc is the allow-listed view of <config>/sessions/<pid>.json. Other keys (cwd, sessionId, sockets and
// the like) are not decoded.
type sessionDoc struct {
	PID             int  `json:"pid"`
	Status          text `json:"status"`
	WaitingFor      text `json:"waitingFor"`
	Name            text `json:"name"`
	SessionID       text `json:"sessionId"` // identity in memory only, never exported
	Kind            text `json:"kind"`
	Entrypoint      text `json:"entrypoint"`
	Tmux            text `json:"tmux"`
	Spare           flag `json:"spare"`
	ProcStart       text `json:"procStart"`
	PidDomain       text `json:"pidDomain"`
	StartedAt       num  `json:"startedAt"`       // epoch milliseconds
	StatusUpdatedAt num  `json:"statusUpdatedAt"` // epoch milliseconds
}

// sessionAlive is true when the pid exists, is the same process the file was written for (start time) and
// belongs to this host (pid domain). A file outlives its process, and a pid can be reused.
func sessionAlive(d sessionDoc) bool {
	if !ProcAlive(d.PID) {
		return false
	}
	if d.ProcStart != "" {
		if got, ok := ProcStart(d.PID); !ok || got != string(d.ProcStart) {
			return false
		}
	}
	if d.PidDomain != "" {
		// A file naming a pid domain is trusted only when this host can name its own and the two are equal; if it
		// cannot, the file may belong to another host sharing the directory.
		if own := PidDomain(); own == "" || own != string(d.PidDomain) {
			return false
		}
	}
	return true
}

// sessionPIDLabel is the decimal pid of a session the checks above prove is this host's live process: the pid exists,
// the start time recorded in the file matches and the recorded pid domain equals this host's. A file that records
// neither, or only one, proves less, so its pid is not published: the label is "" rather than a guess.
func sessionPIDLabel(d sessionDoc) string {
	if d.PID <= 0 || d.ProcStart == "" || d.PidDomain == "" || !sessionAlive(d) {
		return ""
	}
	return strconv.Itoa(d.PID)
}

// Per-session label values are closed sets; nothing from a file reaches a label except the sanitized name.
var (
	sessionStates  = []string{"busy", "idle", "waiting", "shell", "other"}
	waitingReasons = []string{"permission", "input", "dialog", "sandbox", "worker", "goal", "other"}

	// waitingReason is an exact-match allowlist over the harness's waitingFor strings. Free text is never
	// exported: a string not listed here (a new vendor value, or something unexpected) is "other".
	waitingReason = map[string]string{
		"permission prompt": "permission",
		"input needed":      "input",
		"dialog open":       "dialog",
		"sandbox request":   "sandbox",
		"worker request":    "worker",
		"goal proposal":     "goal",
	}
)

func detailState(status string) string {
	switch strings.ToLower(status) {
	case "busy", "running", "active", "working", "thinking":
		return "busy"
	case "idle":
		return "idle"
	case "waiting":
		return "waiting"
	case "shell":
		return "shell"
	}
	return "other"
}

func entrypointLabel(e string) string {
	switch e {
	case "cli":
		return "cli"
	case "sdk-cli":
		return "sdk"
	}
	return "other"
}

var (
	badNameChar = regexp.MustCompile(`[^A-Za-z0-9._-]`)
	tmuxName    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,48}$`)
)

// sessionLabel turns the harness's session name into a bounded label value. In practice the name is the project
// folder name plus a short suffix, so it is a folder name, not a path.
func sessionLabel(name string) string {
	n := badNameChar.ReplaceAllString(name, "-")
	if len(n) > 48 {
		n = n[:48]
	}
	if strings.Trim(n, "-") == "" {
		return "unnamed"
	}
	return n
}

// tmuxSession is the tmux session name: the part of the "session:@window.%pane" value before ":".
func tmuxSession(t string) string {
	name, _, _ := strings.Cut(t, ":")
	if tmuxName.MatchString(name) {
		return name
	}
	return ""
}

// sessionSamples builds the per-session series of one profile. It skips pre-warmed spares and runs that are
// neither interactive nor background, keeps at most max sessions (cli before sdk, then the oldest) and returns how
// many it dropped. Every state and reason is emitted as a fixed 0/1 set, so a state change flips a value and never
// creates a series.
func sessionSamples(p *Profile, account string, docs []sessionDoc, max int, names *nameBook) ([]core.Sample, int) {
	if max <= 0 {
		max = defaultMaxSessions
	}
	var kept []sessionDoc
	for _, d := range docs {
		if d.Spare || (d.Kind != "interactive" && d.Kind != "bg") {
			continue
		}
		kept = append(kept, d)
	}
	byAge := func(s []sessionDoc) func(i, j int) bool {
		return func(i, j int) bool {
			if s[i].StartedAt != s[j].StartedAt {
				return s[i].StartedAt < s[j].StartedAt
			}
			return s[i].PID < s[j].PID
		}
	}
	over := 0
	if len(kept) > max {
		sort.SliceStable(kept, func(i, j int) bool {
			ci, cj := d2rank(kept[i]), d2rank(kept[j])
			if ci != cj {
				return ci < cj
			}
			return byAge(kept)(i, j)
		})
		over = len(kept) - max
		kept = kept[:max]
	}
	sort.SliceStable(kept, byAge(kept))

	labels := names.assign(kept)
	var out []core.Sample
	for i, d := range kept {
		name := labels[i]
		state := detailState(string(d.Status))
		for _, st := range sessionStates {
			out = append(out, core.Sample{Name: "agent_session_state", Labels: p.labels(account,
				core.Label{Name: "session", Value: name}, core.Label{Name: "state", Value: st}), Value: b2f(st == state)})
		}
		reason := "other"
		if r, ok := waitingReason[string(d.WaitingFor)]; ok {
			reason = r
		}
		for _, r := range waitingReasons {
			out = append(out, core.Sample{Name: "agent_session_waiting", Labels: p.labels(account,
				core.Label{Name: "session", Value: name}, core.Label{Name: "reason", Value: r}), Value: b2f(state == "waiting" && r == reason)})
		}
		// Only statusUpdatedAt marks a state change; updatedAt moves on unrelated rewrites of the file.
		if since := d.StatusUpdatedAt; since > 0 {
			out = append(out, core.Sample{Name: "agent_session_state_since_timestamp_seconds",
				Labels: p.labels(account, core.Label{Name: "session", Value: name}), Value: float64(since) / 1000})
		}
		out = append(out, core.Sample{Name: "agent_session_info", Labels: p.labels(account,
			core.Label{Name: "session", Value: name},
			core.Label{Name: "entrypoint", Value: entrypointLabel(string(d.Entrypoint))},
			core.Label{Name: "tmux_session", Value: tmuxSession(string(d.Tmux))},
			core.Label{Name: "pid", Value: sessionPIDLabel(d)}), Value: 1})
	}
	return out, over
}

func d2rank(d sessionDoc) int {
	if d.Entrypoint == "cli" {
		return 0
	}
	return 1
}

// sessionKey identifies one session across polls: a pid is reused only by a process with another start time.
type sessionKey struct {
	pid     int
	started float64
	procRun string // procStart: another process on the same pid
	id      string // the harness's session id, kept in memory only: a new process has a new one
}

func keyOf(d sessionDoc) sessionKey {
	return sessionKey{d.PID, float64(d.StartedAt), string(d.ProcStart), string(d.SessionID)}
}

// nameBook keeps the label each live session was given, so a label never moves to another session while both
// exist: when the older of two sessions with the same name ends, the younger keeps its "-2". New sessions take the
// first free label (the plain name, then -2, -3 ...). It lives in memory only, so labels are re-derived after a
// restart. Entries of sessions that are gone are dropped on the next poll.
type nameBook struct {
	mu   sync.Mutex
	have map[sessionKey]string
}

// assign returns the label of each session in docs (same order), which must be sorted oldest first.
func (n *nameBook) assign(docs []sessionDoc) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	next := make(map[sessionKey]string, len(docs))
	used := map[string]bool{}
	out := make([]string, len(docs))
	for i, d := range docs { // sessions already named keep their label
		k := keyOf(d)
		if l, ok := n.have[k]; ok && !used[l] {
			next[k], used[l], out[i] = l, true, l
		}
	}
	for i, d := range docs {
		if out[i] != "" {
			continue
		}
		k := keyOf(d)
		l := uniqueName(sessionLabel(string(d.Name)), used)
		next[k], used[l], out[i] = l, true, l
	}
	n.have = next
	return out
}

// uniqueName appends -2, -3 ... to a name already taken, keeping the label within 48 characters.
func uniqueName(base string, used map[string]bool) string {
	if !used[base] {
		return base
	}
	for i := 2; ; i++ {
		suffix := "-" + strconv.Itoa(i)
		n := base
		if len(n)+len(suffix) > 48 {
			n = n[:48-len(suffix)]
		}
		if n += suffix; !used[n] {
			return n
		}
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
