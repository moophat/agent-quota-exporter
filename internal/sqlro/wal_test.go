package sqlro

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// sidecars reports which of -wal and -shm exist next to path, with their mtimes.
func sidecars(path string) map[string]time.Time {
	out := map[string]time.Time{}
	for _, s := range []string{"-wal", "-shm"} {
		if fi, err := os.Stat(path + s); err == nil {
			out[s] = fi.ModTime()
		}
	}
	return out
}

// A reader opened per cycle must keep up with a live WAL writer: every committed row seen once, in order.
func TestWALLiveWriter(t *testing.T) {
	path, w := routerDB(t, t.TempDir(), true)
	defer w.Close()
	if s := sidecars(path); len(s) != 2 {
		t.Fatalf("a live WAL writer must have -wal and -shm, got %v", s)
	}
	const extra = 200
	var wg sync.WaitGroup
	wg.Add(1)
	writeErr := make(chan error, 1)
	go func() {
		defer wg.Done()
		for i := 0; i < extra; i++ {
			if _, err := w.Exec(`INSERT INTO usageHistory (provider, model, apiKey, promptTokens) VALUES ('p', 'm', ?, ?)`,
				canary, i); err != nil {
				writeErr <- err
				return
			}
		}
	}()
	var last int64
	seen := 0
	for deadline := time.Now().Add(20 * time.Second); seen < 3+extra; {
		if time.Now().After(deadline) {
			t.Fatalf("reader stalled at %d rows", seen)
		}
		d, err := Open(path, routerAllow)
		if err != nil {
			t.Fatalf("open during writes: %v", err)
		}
		rows, err := d.After("usageHistory", "id", last, usageCols, 7)
		d.Close()
		if err != nil {
			t.Fatalf("read during writes: %v", err)
		}
		for _, r := range rows {
			id := r[0].(int64)
			if id != last+1 {
				t.Fatalf("rows out of order or skipped: %d after %d", id, last)
			}
			last = id
			seen++
		}
		noCanary(t, rows)
	}
	wg.Wait()
	select {
	case err := <-writeErr:
		t.Fatalf("writer: %v", err)
	default:
	}
}

// A WAL database whose owner has exited has no -wal and no -shm: the last writer checkpointed every commit into
// the file and removed both on close. Open must read those rows (an idle database used to be skipped, so the last
// writes before the close were never counted and a fresh source never initialised), and must leave nothing behind.
// Measured with modernc.org/sqlite v1.40.1 (SQLite 3.50.4), under strace: a plain mode=ro reader would open -wal
// and -shm with O_RDWR|O_CREAT and leave them (as root, chowned to the database's owner); mode=ro&immutable=1
// opens only the database, O_RDONLY, takes no lock and looks up no sidecar.
func TestWALSidecarsAbsent(t *testing.T) {
	path, w := routerDB(t, t.TempDir(), true)
	for i := 0; i < 5; i++ {
		if _, err := w.Exec(`INSERT INTO usageHistory (provider, model, apiKey, promptTokens) VALUES ('p', 'late', ?, ?)`,
			canary, 1000+i); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if s := sidecars(path); len(s) != 0 {
		t.Fatalf("closing the last writer should remove -wal and -shm, got %v", s)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Open(path, routerAllow)
	if err != nil {
		t.Fatalf("an idle WAL database must open: %v", err)
	}
	rows, err := d.After("usageHistory", "id", 3, usageCols, 10)
	d.Close()
	if err != nil || len(rows) != 5 || rows[0][2] != "late" || rows[4][3] != int64(1004) {
		t.Fatalf("the rows written just before the last writer closed: %v %v", rows, err)
	}
	noCanary(t, rows)
	if s := sidecars(path); len(s) != 0 {
		t.Fatalf("Open created files in the owner's directory: %v", keys(s))
	}
	after, err := os.Stat(path)
	if err != nil || !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatalf("the database changed under a read-only open: %v -> %v (%v)", before, after, err)
	}
}

// A source that starts while its database is idle initialises from it: every row, not ErrIdle.
func TestWALIdleFreshOpen(t *testing.T) {
	path, w := routerDB(t, t.TempDir(), true)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	d := mustOpen(t, path, routerAllow)
	if n, err := d.MaxInt("usageHistory", "id"); err != nil || n != 3 {
		t.Fatalf("max id %d %v", n, err)
	}
	if s := sidecars(path); len(s) != 0 {
		t.Fatalf("Open created %v", keys(s))
	}
}

// Exactly one sidecar is not a state a writer leaves (it creates both and removes both): Open returns ErrIdle and
// creates the missing one neither.
func TestWALOneSidecarIsIdle(t *testing.T) {
	for _, have := range []string{"-wal", "-shm"} {
		t.Run(have, func(t *testing.T) {
			path, w := routerDB(t, t.TempDir(), true)
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+have, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path, routerAllow); !errors.Is(err, ErrIdle) {
				t.Fatalf("want ErrIdle with only %s, got %v", have, err)
			}
			if s := sidecars(path); len(s) != 1 {
				t.Fatalf("Open created a sidecar: %v", keys(s))
			}
		})
	}
}

// Read-only directory (measured as an unprivileged user; root ignores directory modes, so the test skips as
// root). Without -wal and -shm the database is read immutable, which needs no sidecar and creates none (a plain
// mode=ro open would fail here with SQLITE_READONLY_DIRECTORY, measured before immutable reads). With a live writer
// the sidecars exist and the read works.
func TestWALReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions; run the compiled test binary as an unprivileged user")
	}
	roDir := func(t *testing.T) string {
		dir := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
		return dir
	}
	t.Run("no sidecars", func(t *testing.T) {
		dir := roDir(t)
		path, w := routerDB(t, dir, true)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		d := mustOpen(t, path, routerAllow)
		if rows, err := d.After("usageHistory", "id", 0, usageCols, 10); err != nil || len(rows) != 3 {
			t.Fatalf("read: %d rows, %v", len(rows), err)
		}
		if s := sidecars(path); len(s) != 0 {
			t.Fatalf("sidecars appeared: %v", keys(s))
		}
	})
	t.Run("live writer", func(t *testing.T) {
		dir := roDir(t)
		path, w := routerDB(t, dir, true)
		defer w.Close()
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		d := mustOpen(t, path, routerAllow)
		if rows, err := d.After("usageHistory", "id", 0, usageCols, 10); err != nil || len(rows) != 3 {
			t.Fatalf("read: %d rows, %v", len(rows), err)
		}
	})
}

func keys(m map[string]time.Time) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestHeap13DBs measures memory for a host with 13 databases opened, read and closed every cycle. It reports
// numbers only: Go heap after GC, plus process RSS, since SQLite's page cache is allocated outside the Go heap.
func TestHeap13DBs(t *testing.T) {
	src, err := os.ReadFile(opencodeDB(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var paths []string
	for i := 0; i < 13; i++ {
		p := filepath.Join(dir, "db"+string(rune('a'+i))+".db")
		if err := os.WriteFile(p, src, 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	cycle := func() {
		for _, p := range paths {
			d, err := Open(p, opencodeAllow)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.After("message", "rowid", 0, msgCols, 1000); err != nil {
				t.Fatal(err)
			}
			if _, err := d.MaxRowid("message"); err != nil {
				t.Fatal(err)
			}
			d.Close()
		}
	}
	report := func(label string) {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		t.Logf("%s: HeapInuse=%d KiB HeapObjects=%d Sys=%d KiB %s", label, m.HeapInuse>>10, m.HeapObjects, m.Sys>>10, rss())
	}
	report("before")
	cycle()
	report("after 1 cycle")
	for i := 0; i < 50; i++ {
		cycle()
	}
	report("after 51 cycles")
}

func rss() string {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			return "RSS=" + strings.Join(strings.Fields(l)[1:], " ")
		}
	}
	return ""
}
