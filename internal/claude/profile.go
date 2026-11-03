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
	"sync"
	"syscall"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

const (
	Harness  = "claude-code"
	Provider = "anthropic"
)

// Profile is one CLAUDE_CONFIG_DIR. All plugins of a profile share it, so they agree on the account label.
type Profile struct {
	Host         string
	Alias        string
	Dir          string
	AccountAlias string // operator alias from config; "" = derive
	EmailLabel   bool   // metrics.account_label: email
	MaxModels    int

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
	return p.label, p.uuid
}

// ResolveAccount re-reads .claude.json when its mtime changed (specs/sources.md: "at start, on change").
// Only oauthAccount.accountUuid (and emailAddress in email mode) is decoded.
func (p *Profile) ResolveAccount() error {
	path := ClaudeJSONPath(p.Dir)
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

// ClaudeJSONPath: <dir>/.claude.json, or for a default ~/.claude dir the sibling ~/.claude.json.
func ClaudeJSONPath(dir string) string {
	in := filepath.Join(dir, ".claude.json")
	if _, err := os.Stat(in); err == nil || filepath.Base(dir) != ".claude" {
		return in
	}
	return filepath.Join(filepath.Dir(dir), ".claude.json")
}

func (p *Profile) labels(account string, extra ...core.Label) []core.Label {
	ls := []core.Label{{Name: "host", Value: p.Host}, {Name: "harness", Value: Harness}, {Name: "profile", Value: p.Alias}, {Name: "account", Value: account}}
	return append(ls, extra...)
}

// openRegular opens a file read-only without following a final symlink and without blocking on FIFOs or
// devices, and refuses anything that is not a regular file (a profile dir may be writable by its user while the
// exporter runs as root).
func openRegular(path string) (*os.File, os.FileInfo, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, nil, core.Fail(core.ReasonPermission)
		}
		return nil, nil, core.Fail(core.ErrReason(&os.PathError{Err: err}))
	}
	f := os.NewFile(uintptr(fd), path)
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, core.Fail(core.ReasonOther)
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, core.Fail(core.ReasonPermission)
	}
	return f, fi, nil
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
		ExpiresAt   int64   `json:"expiresAt"` // Unix milliseconds
		AccessToken present `json:"accessToken"`
	} `json:"claudeAiOauth"`
}

// present records whether a JSON value is a non-empty string, without keeping the value.
type present bool

func (p *present) UnmarshalJSON(b []byte) error {
	*p = present(len(b) > 2 && b[0] == '"')
	return nil
}

// readCredentialFacts returns (token present, expiry). A missing file is a valid logged-out state.
func readCredentialFacts(dir string) (bool, time.Time, error) {
	b, err := readSmall(filepath.Join(dir, ".credentials.json"), 1<<20)
	if err != nil {
		var se *core.SourceError
		if errors.As(err, &se) && se.Reason == core.ReasonNotFound {
			return false, time.Time{}, nil
		}
		return false, time.Time{}, err
	}
	defer wipe(b)
	var c credentials
	if json.Unmarshal(b, &c) != nil {
		return false, time.Time{}, core.Fail(core.ReasonParse)
	}
	if c.OAuth == nil {
		return false, time.Time{}, nil
	}
	var exp time.Time
	if c.OAuth.ExpiresAt > 0 {
		exp = time.UnixMilli(c.OAuth.ExpiresAt)
	}
	return bool(c.OAuth.AccessToken), exp, nil
}
