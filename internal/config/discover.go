package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Resolved is one profile the exporter will collect from.
type Resolved struct {
	Harness    string
	Alias      string
	Dir        string // real path (symlinks resolved)
	ConfigDir  string // the path as configured or discovered
	Account    string // operator alias, "" = derive
	QuotaOptIn bool
	Origin     string // config | discovered
}

// Resolve merges config profiles with discovery per discovery.mode. It returns the profiles to collect,
// and warnings for the caller to log once (skipped harnesses, unreadable dirs).
func (c *Config) Resolve(env func(string) string) ([]Resolved, []string) {
	var out []Resolved
	var warns []string
	taken := map[string]bool{} // real dir paths
	aliases := map[string]bool{}

	if c.Discovery.Mode != "only" {
		for _, p := range c.Profiles {
			if p.Disable {
				continue
			}
			if p.Harness != HarnessClaude {
				warns = append(warns, fmt.Sprintf("profile %s/%s: harness %q has no plugin in this version; skipped", p.Harness, p.Alias, p.Harness))
				continue
			}
			real := realPath(p.Dir)
			taken[real] = true
			aliases[p.Alias] = true
			out = append(out, Resolved{Harness: p.Harness, Alias: p.Alias, Dir: real, ConfigDir: p.Dir, Account: p.Account,
				QuotaOptIn: p.Quota["claude_oauth_usage"], Origin: "config"})
		}
	}
	// A disabled config profile also suppresses discovery of the same dir.
	for _, p := range c.Profiles {
		if p.Disable {
			taken[realPath(p.Dir)] = true
		}
	}
	if c.Discovery.Mode == "off" {
		return out, warns
	}

	var found []string
	if d := env("CLAUDE_CONFIG_DIR"); d != "" && isClaudeDir(d) {
		found = append(found, d)
	}
	for _, root := range c.Discovery.Roots {
		homes, err := filepath.Glob(root)
		if err != nil {
			warns = append(warns, fmt.Sprintf("discovery root %q: bad pattern", root))
			continue
		}
		for _, h := range homes {
			ents, err := os.ReadDir(h)
			if err != nil {
				continue
			}
			for _, e := range ents {
				n := e.Name()
				if n != ".claude" && !strings.HasPrefix(n, ".claude-") {
					continue
				}
				d := filepath.Join(h, n)
				// A discovered entry must be a real directory: as root, following a user's link could expose
				// another profile under their name. Operator-listed dirs (config, CLAUDE_CONFIG_DIR) may be links.
				if e.Type()&os.ModeSymlink != 0 {
					continue
				}
				if isClaudeDir(d) {
					found = append(found, d)
				}
			}
		}
	}
	type cand struct{ dir, real, alias string }
	var cands []cand
	for _, d := range found {
		r := realPath(d)
		if taken[r] {
			continue
		}
		taken[r] = true
		cands = append(cands, cand{d, r, discoveredAlias(d)})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].real < cands[j].real })
	for _, cd := range cands {
		a := cd.alias
		for i := 2; aliases[a]; i++ {
			a = fmt.Sprintf("%s-%d", cd.alias, i)
		}
		aliases[a] = true
		out = append(out, Resolved{Harness: HarnessClaude, Alias: a, Dir: cd.real, ConfigDir: cd.dir, Origin: "discovered"})
	}
	return out, warns
}

// isClaudeDir: a directory holding at least one Claude Code marker.
func isClaudeDir(d string) bool {
	st, err := os.Stat(d)
	if err != nil || !st.IsDir() {
		return false
	}
	for _, m := range []string{"projects", "sessions", ".credentials.json"} {
		if _, err := os.Stat(filepath.Join(d, m)); err == nil {
			return true
		}
	}
	return false
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

var nonSlug = regexp.MustCompile(`[^a-z0-9-]+`)

// Slug lowercases s and replaces runs of characters outside [a-z0-9-] with "-".
func Slug(s string) string {
	s = nonSlug.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "profile"
	}
	return s
}

// discoveredAlias: "<home>/.claude" → slug(home name); "<home>/.claude-X" → slug("claude-X").
// The spec's "slug of the directory name" would make every ~/.claude collide as "claude"; the home name
// keeps aliases stable and distinct (deviation recorded in the plan).
func discoveredAlias(d string) string {
	base := filepath.Base(d)
	if base == ".claude" {
		return Slug(filepath.Base(filepath.Dir(d)))
	}
	return Slug(strings.TrimPrefix(base, "."))
}
