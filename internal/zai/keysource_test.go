package zai

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

const canaryKey = "CANARYZAI0000000000000000.CANARYZAI00000000"

// holderDir returns a temp dir holding name with body.
func holderDir(t *testing.T, name, body string) string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

func fixtureBody(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "zai", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func reasonOf(t *testing.T, err error) string {
	t.Helper()
	var se *core.SourceError
	if !errors.As(err, &se) {
		t.Fatalf("want a SourceError, got %v", err)
	}
	return se.Reason
}

func wantReason(t *testing.T, err error, reason string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s, got success", reason)
	}
	if got := reasonOf(t, err); got != reason {
		t.Fatalf("want %s, got %s", reason, got)
	}
}

func TestReaderFor(t *testing.T) {
	for _, s := range []string{"openclaw", "opencode", "zai-helper"} {
		if _, ok := readerFor(s); !ok {
			t.Fatalf("no reader for %s", s)
		}
	}
	for _, s := range []string{"zcode", "", "nope"} {
		if _, ok := readerFor(s); ok {
			t.Fatalf("reader for %q", s)
		}
	}
}

func TestKeyZeroWipes(t *testing.T) {
	k := key{value: []byte(canaryKey)}
	buf := k.value
	k.Zero()
	if k.value != nil {
		t.Fatal("value kept")
	}
	if !bytes.Equal(buf, make([]byte, len(buf))) {
		t.Fatal("buffer not wiped")
	}
}

func TestKeyShape(t *testing.T) {
	for name, v := range map[string]string{
		"space": "CANARYZAI00000000 00000000", "newline": "CANARYZAI0000000000000000\n", "cr": "CANARYZAI0000000000\r0000",
		"non-ascii": "CANARYZAI00000000000000é0", "short": "CANARYZAI00000", "long": strings.Repeat("A", 513), "control": "CANARYZAI0000000000\x01000000",
	} {
		if validKey([]byte(v)) {
			t.Errorf("%s accepted", name)
		}
	}
	for _, v := range []string{canaryKey, strings.Repeat("A", 16), strings.Repeat("A", 512)} {
		if !validKey([]byte(v)) {
			t.Errorf("%d bytes rejected", len(v))
		}
	}
}

// holderCases runs the common holder-file behaviour against each file-based reader.
func TestHolderFileCommon(t *testing.T) {
	good := map[string]string{"openclaw": fixtureBody(t, "openclaw.json5"), "zai-helper": fixtureBody(t, "chelper-config.yaml")}
	files := map[string]string{"openclaw": "openclaw.json", "zai-helper": "config.yaml"}
	for src, body := range good {
		rd, _ := readerFor(src)
		name := files[src]
		t.Run(src+"/ok", func(t *testing.T) {
			k, err := rd(holderDir(t, name, body))
			if err != nil {
				t.Fatal(err)
			}
			if k.fp == "" || len(k.value) == 0 {
				t.Fatalf("empty key or fp")
			}
			k.Zero()
		})
		t.Run(src+"/missing dir", func(t *testing.T) {
			_, err := rd(filepath.Join(t.TempDir(), "absent"))
			wantReason(t, err, core.ReasonNotFound)
		})
		t.Run(src+"/missing file", func(t *testing.T) {
			_, err := rd(t.TempDir())
			wantReason(t, err, core.ReasonNotFound)
		})
		t.Run(src+"/dir is link", func(t *testing.T) {
			real := holderDir(t, name, body)
			link := filepath.Join(t.TempDir(), "l")
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			_, err := rd(link)
			wantReason(t, err, core.ReasonPermission)
		})
		t.Run(src+"/file is link", func(t *testing.T) {
			real := holderDir(t, "elsewhere", body)
			d := t.TempDir()
			if err := os.Symlink(filepath.Join(real, "elsewhere"), filepath.Join(d, name)); err != nil {
				t.Fatal(err)
			}
			_, err := rd(d)
			wantReason(t, err, core.ReasonPermission)
		})
		t.Run(src+"/file is dir", func(t *testing.T) {
			d := t.TempDir()
			if err := os.Mkdir(filepath.Join(d, name), 0o700); err != nil {
				t.Fatal(err)
			}
			_, err := rd(d)
			wantReason(t, err, core.ReasonPermission)
		})
		t.Run(src+"/file is fifo", func(t *testing.T) {
			d := t.TempDir()
			if err := syscall.Mkfifo(filepath.Join(d, name), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := rd(d)
			wantReason(t, err, core.ReasonPermission)
		})
		t.Run(src+"/over 1 MiB", func(t *testing.T) {
			_, err := rd(holderDir(t, name, body+"\n# "+strings.Repeat("x", 1<<20)))
			wantReason(t, err, core.ReasonParse)
		})
		t.Run(src+"/fp changes on rewrite", func(t *testing.T) {
			d := holderDir(t, name, body)
			k1, err := rd(d)
			if err != nil {
				t.Fatal(err)
			}
			fp1 := k1.fp
			k1.Zero()
			if err := os.WriteFile(filepath.Join(d, name), []byte(body+"\n\n\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			k2, err := rd(d)
			if err != nil {
				t.Fatal(err)
			}
			defer k2.Zero()
			if k2.fp == fp1 {
				t.Fatal("fp unchanged")
			}
		})
	}
}

// Every error text is free of the key and the path.
func TestReaderErrorsLeakNothing(t *testing.T) {
	dirs := map[string]string{
		"openclaw":   holderDir(t, "openclaw.json", `{"models":{"providers":{"zai":{"apiKey":"`+canaryKey+` bad"}}}}`),
		"zai-helper": holderDir(t, "config.yaml", "api_key: \""+canaryKey+" bad\"\n"),
	}
	for src, d := range dirs {
		rd, _ := readerFor(src)
		_, err := rd(d)
		if err == nil {
			t.Fatalf("%s: want error", src)
		}
		if s := err.Error(); strings.Contains(s, "CANARY") || strings.Contains(s, d) {
			t.Fatalf("%s: error leaks: %s", src, s)
		}
	}
	rd, _ := readerFor("openclaw")
	_, err := rd(filepath.Join(t.TempDir(), "absent"))
	if strings.Contains(err.Error(), os.TempDir()) {
		t.Fatal("path in error")
	}
}
