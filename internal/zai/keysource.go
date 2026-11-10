// Package zai reads the Coding Plan quota of a z.ai account with a key a local harness already holds. The exporter
// never asks for a key: it reads the holder's own file, read-only, for one request, and never logs or stores it.
package zai

import (
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/safefs"
)

const maxHolderFile = 1 << 20

// key is request-local. fp identifies the file revision it came from (never a hash of the key).
type key struct {
	value []byte
	fp    string
}

// Zero wipes the key bytes.
func (k *key) Zero() {
	for i := range k.value {
		k.value[i] = 0
	}
	k.value = nil
}

// reader returns the key held under dir. Its errors are core.Fail(reason) only: no path, no text of the file.
type reader func(dir string) (key, error)

func readerFor(source string) (reader, bool) {
	switch source {
	case "openclaw":
		return readOpenclaw, true
	case "opencode":
		return readOpencode, true
	case "zai-helper":
		return readHelper, true
	}
	return nil, false
}

// validKey is the shape check: printable ASCII without whitespace, 16..512 bytes.
func validKey(b []byte) bool {
	if len(b) < 16 || len(b) > 512 {
		return false
	}
	for _, c := range b {
		if c <= 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

// openHolderFile reads dir/name without following links. The caller zeroes the returned bytes.
func openHolderFile(dir, name string) ([]byte, string, error) {
	if err := safefs.RealDir(dir); err != nil {
		return nil, "", err
	}
	f, fi, err := safefs.OpenRegular(filepath.Join(dir, name))
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	if fi.Size() > maxHolderFile {
		return nil, "", core.Fail(core.ReasonParse)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxHolderFile+1))
	if err != nil {
		return nil, "", core.Fail(core.ReasonOther)
	}
	if len(b) > maxHolderFile {
		wipe(b)
		return nil, "", core.Fail(core.ReasonParse)
	}
	return b, fmt.Sprintf("%d:%d", fi.ModTime().UnixNano()/int64(time.Millisecond), fi.Size()), nil
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// finishKey applies the shape check to a decoded candidate and copies it into a wipeable key.
func finishKey(s, fp string) (key, error) {
	if !validKey([]byte(s)) {
		return key{}, core.Fail(core.ReasonParse)
	}
	return key{value: []byte(s), fp: fp}, nil
}
