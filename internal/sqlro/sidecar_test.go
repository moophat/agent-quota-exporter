package sqlro

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// TestHelperWALWriter is not a test of its own: liveWriter runs the test binary again with only this test
// selected, to get a WAL writer in another process, as the owning program is. (Connections of one process share
// SQLite's lock state and -shm, so an in-process writer would hide both from the reader.)
func TestHelperWALWriter(t *testing.T) {
	path := os.Getenv("SQLRO_HELPER_WAL_WRITER")
	if path == "" {
		t.Skip("helper process for liveWriter")
	}
	w := writer(t, path, true)
	defer w.Close()
	var n int
	if err := w.QueryRow("SELECT count(*) FROM usageHistory").Scan(&n); err != nil {
		t.Fatal(err)
	}
	// A write lock on SQLite's PENDING byte (offset 0x40000000): a reader that reaches SQLite's locking gets
	// SQLITE_BUSY and, after the 200 ms busy timeout, core.ReasonTimeout. A fast core.ReasonPermission therefore
	// proves Open refused before SQLite ran. f stays open: closing any descriptor of the file would drop every
	// POSIX lock this process holds on it, SQLite's included.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart, Start: 0x40000000, Len: 1}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk); err != nil {
		t.Fatal(err)
	}
	fmt.Println("ready")
	io.Copy(io.Discard, os.Stdin) // until the parent closes stdin
}

// liveWriter keeps path open in WAL mode from another process until the test ends; both sidecars then exist.
func liveWriter(t *testing.T, path string) {
	t.Helper()
	// /proc/self/exe, not os.Args[0]: the binary may sit in a directory the test user cannot search (it was started
	// by root through setpriv).
	cmd := exec.Command("/proc/self/exe", "-test.run=^TestHelperWALWriter$", "-test.count=1")
	cmd.Env = append(os.Environ(), "SQLRO_HELPER_WAL_WRITER="+path)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(out)
	t.Cleanup(func() {
		in.Close()
		io.Copy(io.Discard, r)
		cmd.Wait()
	})
	if line, err := r.ReadString('\n'); line != "ready\n" {
		t.Fatalf("helper writer did not start: %q %v", line, err)
	}
	if s := sidecars(path); len(s) != 2 {
		t.Fatalf("a live WAL writer must have -wal and -shm, got %v", keys(s))
	}
}

// The exporter may run as root, and SQLite opens -wal and -shm by name with O_RDWR|O_CREAT and fchowns them to the
// database's owner. The owner of the directory could plant a sidecar that leads elsewhere; Open must refuse any
// sidecar that is not a plain regular file before SQLite runs, and the target must not change in any way.
func TestSidecarNotRegular(t *testing.T) {
	cases := []struct {
		name, side string
		plant      func(at, canary string) error
	}{
		{"symlinked -shm", "-shm", func(at, canary string) error { return os.Symlink(canary, at) }},
		{"symlinked -wal", "-wal", func(at, canary string) error { return os.Symlink(canary, at) }},
		{"hard-linked -shm", "-shm", func(at, canary string) error { return os.Link(canary, at) }},
		{"directory -wal", "-wal", func(at, _ string) error { return os.Mkdir(at, 0o755) }},
		{"FIFO -shm", "-shm", func(at, _ string) error { return syscall.Mkfifo(at, 0o644) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			canary := filepath.Join(t.TempDir(), "canary")
			content := []byte("canary: must never be written, truncated or chowned\n")
			if err := os.WriteFile(canary, content, 0o644); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-time.Hour).Truncate(time.Second)
			if err := os.Chtimes(canary, old, old); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(canary)
			if err != nil {
				t.Fatal(err)
			}

			path, w := routerDB(t, t.TempDir(), true)
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			liveWriter(t, path)
			// The writer keeps its descriptors; only the name in the owner's directory is replaced.
			if err := os.Remove(path + c.side); err != nil {
				t.Fatal(err)
			}
			if err := c.plant(path+c.side, canary); err != nil {
				t.Fatal(err)
			}

			start := time.Now()
			_, err = Open(path, routerAllow)
			if el := time.Since(start); reasonOf(err) != core.ReasonPermission || el > 100*time.Millisecond {
				t.Fatalf("want a fast %s before SQLite runs, got %v after %v", core.ReasonPermission, err, el)
			}

			got, err := os.ReadFile(canary)
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("canary changed: %q %v", got, err)
			}
			after, err := os.Stat(canary)
			if err != nil {
				t.Fatal(err)
			}
			bs, as := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
			if !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() || as.Uid != bs.Uid || as.Gid != bs.Gid {
				t.Fatalf("canary metadata changed: %v uid %d gid %d -> %v uid %d gid %d",
					before, bs.Uid, bs.Gid, after, as.Uid, as.Gid)
			}
		})
	}
}

// The post-open check: a path that no longer leads to the file checked before SQLite opened it is refused.
func TestSameFileAfterSwap(t *testing.T) {
	dir := t.TempDir()
	path := opencodeDB(t, dir)
	other := opencodeDB(t, t.TempDir())
	id, immutable, err := inspect(path)
	if err != nil || immutable {
		t.Fatalf("inspect: %v %v", immutable, err)
	}
	if err := sameFile(path, id); err != nil {
		t.Fatalf("unchanged path: %v", err)
	}
	if err := os.Rename(other, path); err != nil {
		t.Fatal(err)
	}
	if err := sameFile(path, id); reasonOf(err) != core.ReasonPermission {
		t.Fatalf("swapped file: want %s, got %v", core.ReasonPermission, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, path); err != nil {
		t.Fatal(err)
	}
	if err := sameFile(path, id); reasonOf(err) != core.ReasonPermission {
		t.Fatalf("link at the path: want %s, got %v", core.ReasonPermission, err)
	}
}
