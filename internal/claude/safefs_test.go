package claude

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A link anywhere on the path, not only at its end, is refused (Qodo #3).
func TestNoLinkFollowedAtAnyLevel(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	write(t, filepath.Join(real, "inner", "f.json"), []byte(`{}`))
	os.Symlink(real, filepath.Join(base, "link"))

	if _, err := openDir(filepath.Join(base, "link", "inner")); err == nil {
		t.Fatal("intermediate link followed by openDir")
	}
	if _, _, err := openRegular(filepath.Join(base, "link", "inner", "f.json")); err == nil {
		t.Fatal("intermediate link followed by openRegular")
	}
	if f, _, err := openRegular(filepath.Join(real, "inner", "f.json")); err != nil {
		t.Fatalf("plain path refused: %v", err)
	} else {
		f.Close()
	}
}

// A profile whose home was swapped for a link after start cannot be read through it.
func TestProfileBehindSwappedHomeRefused(t *testing.T) {
	p := newProfile(t, "a")
	home := filepath.Dir(p.Dir)
	moved := home + "-moved"
	if err := os.Rename(home, moved); err != nil {
		t.Fatal(err)
	}
	os.Symlink(moved, home)
	if _, _, err := collect(t, &AccountSource{P: p, Interval: time.Minute}, t0, nil); err == nil {
		t.Fatal("read through a swapped-in link")
	}
}

// The transcript walk never descends into a linked sub-directory of projects/.
func TestTranscriptsWalkSkipsLinkedSubdir(t *testing.T) {
	x := newTT(t)
	x.collect()
	elsewhere := t.TempDir()
	write(t, filepath.Join(elsewhere, "s.jsonl"), asstLine("l1", "model-x", "2026-01-01T00:01:00Z", 5))
	os.Symlink(elsewhere, filepath.Join(x.p.Dir, "projects", "-linked"))
	if got := x.collect(); got["agent_tokens_total:model-x:input"] != 0 {
		t.Fatalf("followed a linked project dir: %v", got)
	}
}
