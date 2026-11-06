// Package claude holds the Claude Code harness plugins (specs/sources.md, "Claude Code").
package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/events"
)

const (
	Harness  = "claude-code"
	Provider = "anthropic"
)

// Profile is one CLAUDE_CONFIG_DIR. All plugins of a profile share it, so they agree on the account label.
type Profile struct {
	Host         string
	Alias        string
	Dir          string // real path (symlinks resolved at start)
	ConfigDir    string // the path as configured or discovered; decides where a default install keeps .claude.json
	AccountAlias string // operator alias from config; "" = derive
	EmailLabel   bool   // metrics.account_label: email
	MaxModels    int
	Events       *events.Store
	IdentityKey  []byte
	history      []identitySample
	pending      json.RawMessage

	mu       sync.Mutex
	resolved bool
	jsonMod  time.Time
	uuid     string
	label    string
}

// Account returns the account label and uuid, resolving them first if no plugin has yet.
func (p *Profile) Account() (label, uuid string) {
	p.mu.Lock()
	done := p.resolved
	p.mu.Unlock()
	if !done {
		_ = p.ResolveAccount()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Events != nil && !p.EmailLabel && len(p.history) > 0 {
		account := p.history[len(p.history)-1].data.Account
		if account != nil {
			return *account, p.uuid
		}
		return "unknown", p.uuid
	}
	return p.label, p.uuid
}

// ResolveAccount re-reads .claude.json when its mtime changed (specs/sources.md: "at start, on change").
// Only oauthAccount.accountUuid (and emailAddress in email mode) is decoded.
func (p *Profile) ResolveAccount() error {
	path := ClaudeJSONPath(p.Dir, p.ConfigDir)
	fi, statErr := os.Stat(path)
	p.mu.Lock()
	if p.resolved && statErr == nil && fi.ModTime().Equal(p.jsonMod) {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	var uuid, email string
	var mod time.Time
	var rerr error
	if statErr == nil {
		mod = fi.ModTime()
		b, err := readSmall(path, 64<<20)
		if err != nil {
			rerr = err
		} else {
			var doc struct {
				OAuthAccount *struct {
					AccountUUID string `json:"accountUuid"`
					Email       string `json:"emailAddress"`
				} `json:"oauthAccount"`
			}
			if json.Unmarshal(b, &doc) != nil {
				rerr = core.Fail(core.ReasonParse)
			} else if doc.OAuthAccount != nil {
				uuid = doc.OAuthAccount.AccountUUID
				if p.EmailLabel {
					email = doc.OAuthAccount.Email
				}
			}
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		rerr = core.Fail(core.ErrReason(statErr))
	}

	label := "unknown"
	switch {
	case p.AccountAlias != "":
		label = p.AccountAlias
	case email != "":
		label = email
	case uuid != "":
		h := sha256.Sum256([]byte(uuid))
		label = "acct-" + hex.EncodeToString(h[:])[:8]
	}
	p.mu.Lock()
	p.resolved, p.jsonMod, p.uuid, p.label = true, mod, uuid, label
	p.mu.Unlock()
	return rerr
}

// ClaudeJSONPath: <dir>/.claude.json, or for a default ~/.claude dir the sibling ~/.claude.json. configDir is
// the path as given (it may be a symlink named .claude whose target has another name); "" means dir.
func ClaudeJSONPath(dir, configDir string) string {
	in := filepath.Join(dir, ".claude.json")
	if configDir == "" {
		configDir = dir
	}
	if _, err := os.Stat(in); err == nil || filepath.Base(configDir) != ".claude" {
		return in
	}
	return filepath.Join(filepath.Dir(configDir), ".claude.json")
}

func (p *Profile) labels(account string, extra ...core.Label) []core.Label {
	ls := []core.Label{{Name: "host", Value: p.Host}, {Name: "harness", Value: Harness}, {Name: "profile", Value: p.Alias}, {Name: "account", Value: account}}
	return append(ls, extra...)
}

// readSmall reads a whole regular file of at most max bytes.
func readSmall(path string, max int64) ([]byte, error) {
	f, fi, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi.Size() > max {
		return nil, core.Fail(core.ReasonParse)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, core.Fail(core.ReasonOther)
	}
	return b, nil
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// credentials is the allow-listed view of .credentials.json: expiry and token presence only.
type credentials struct {
	OAuth *struct {
		ExpiresAt        int64           `json:"expiresAt"` // Unix milliseconds
		AccessToken      present         `json:"accessToken"`
		SubscriptionType json.RawMessage `json:"subscriptionType"` // decoded leniently: a non-string is unknown
	} `json:"claudeAiOauth"`
}

// plans is the bounded vocabulary of agent_account_info{plan}; any other subscription type reads as unknown, so a new
// upstream plan name can never add an unbounded label value.
var plans = map[string]bool{"pro": true, "max": true, "team": true}

// present records whether a JSON value is a non-empty string, without keeping the value.
type present bool

func (p *present) UnmarshalJSON(b []byte) error {
	*p = present(len(b) > 2 && b[0] == '"')
	return nil
}

// readCredentialFacts returns (token present, expiry, plan). A missing file is a valid logged-out state.
func readCredentialFacts(dir string) (bool, time.Time, string, error) {
	b, err := readSmall(filepath.Join(dir, ".credentials.json"), 1<<20)
	if err != nil {
		var se *core.SourceError
		if errors.As(err, &se) && se.Reason == core.ReasonNotFound {
			return false, time.Time{}, "unknown", nil
		}
		return false, time.Time{}, "", err
	}
	defer wipe(b)
	var c credentials
	if json.Unmarshal(b, &c) != nil {
		return false, time.Time{}, "", core.Fail(core.ReasonParse)
	}
	if c.OAuth == nil {
		return false, time.Time{}, "unknown", nil
	}
	var exp time.Time
	if c.OAuth.ExpiresAt > 0 {
		exp = time.UnixMilli(c.OAuth.ExpiresAt)
	}
	var plan string
	if json.Unmarshal(c.OAuth.SubscriptionType, &plan) == nil {
		plan = strings.ToLower(plan)
	}
	if !plans[plan] {
		plan = "unknown"
	}
	return bool(c.OAuth.AccessToken), exp, plan, nil
}
