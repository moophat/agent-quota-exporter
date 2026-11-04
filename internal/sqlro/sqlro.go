// Package sqlro reads SQLite databases owned by other programs (opencode, a model router), read-only and only
// through a static allow-list.
//
// The allow-list contract:
//
//   - A plugin declares a static Allow: table name -> the column expressions it may read. An expression is a
//     plain column (Col) or one scalar inside a JSON column (JSON{Col, Path}). The rowid of a listed table is
//     always allowed, as a cursor and as a selected column.
//   - There is no raw SQL entry point. The only queries are After (rows after a cursor), ByKeys (rows by key),
//     MaxRowid and MaxInt. Every table, column and JSON path is checked against the allow-list and against a
//     strict identifier or path pattern before any SQL string is built. Anything else fails with
//     core.ReasonOther and runs no query. Identifiers are double-quoted.
//   - A JSON expression only ever returns a scalar: an object or array comes back NULL, malformed JSON comes
//     back NULL, and text is cut to 128 characters. A plain text or blob column is cut to 128 characters too.
//   - A bare Col returns the column value itself. A plugin must therefore never list a bare column that holds a
//     JSON blob or a secret (tokens, keys, credential values, message content); it lists JSON{Col, Path} for each
//     scalar field it needs instead.
//   - Errors are *core.SourceError carrying a reason only: no driver text, path or SQL ever leaves the package.
//
// Opening (measured with modernc.org/sqlite v1.40.1, SQLite 3.50.4, under strace; 2026-11-04). The exporter may
// run as root over directories owned by untrusted users, and SQLite opens the database, -wal and -shm by path
// name: the database O_RDONLY|O_NOFOLLOW, both sidecars O_RDWR|O_CREAT|O_NOFOLLOW, and as root it fchowns each
// sidecar to the database's owner. O_NOFOLLOW covers the last component only. Open therefore, before SQLite runs:
//
//   - walks the path from "/" without following any link (internal/safefs) and requires a regular file;
//   - checks -wal and -shm in that same pinned directory: each must be absent, or a regular file with one link.
//     A link, directory, FIFO or hard link there is refused with core.ReasonPermission, whatever the header says
//     (SQLite enters WAL mode whenever a -wal exists);
//   - picks the mode from the header (bytes 18 and 19 are 2 for WAL) and the sidecars:
//     a rollback-journal database, or a WAL database with both sidecars (a writer has it open), opens as
//     file:<path>?mode=ro; a WAL database with neither sidecar (no writer; the last one checkpointed everything
//     into the file and removed both on close) opens as mode=ro&immutable=1, which takes no lock and opens
//     nothing but the database, O_RDONLY (measured: no sidecar is looked up, created or written); a WAL
//     database with exactly one sidecar is an odd state and returns ErrIdle.
//
// Every connection gets query_only, a 200 ms busy timeout, a 256 KiB page cache, no mmap and a single
// connection. Callers open once per collect cycle and Close after, so no file handle or cache outlives the cycle.
// A lock held by the owning program surfaces as core.ReasonTimeout: the caller retries next cycle instead of
// waiting.
//
// Residual risks, stated plainly:
//
//   - Path swap. Pinning the checked directory for SQLite does not work: given /proc/self/fd/<dirfd>/<name>,
//     SQLite canonicalises the path in user space (lstat on each component, readlink on each link) and opens the
//     original path string again (measured). So the checks above and SQLite's open are separate path walks. After
//     SQLite has opened the database, Open walks the path again without following links and requires the same
//     device and inode it checked; a mismatch closes the connection and fails with core.ReasonPermission. A
//     component swapped for a link and swapped back within SQLite's open, or a sidecar swapped for a hard link in
//     that window (only possible where fs.protected_hardlinks is 0), is not caught.
//   - Immutable read. A writer that starts while an immutable read runs is ignored by it (stale values until next
//     cycle); if that writer also checkpoints into the file during the read, pages can be torn and rows may parse
//     as errors. Reads are bounded (see maxLimit) and connections live one cycle, which keeps the window short.
//   - A writer that closes between the sidecar check and SQLite's open can leave fresh sidecars behind (created by
//     this reader, owned by the database's owner); the window is one open.
package sqlro

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"syscall"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/safefs"
)

// pragmas are applied to every connection (busy_timeout first, by the driver).
const pragmas = "_pragma=busy_timeout(200)&_pragma=query_only(1)&_pragma=cache_size(-256)&_pragma=mmap_size(0)"

// ErrIdle means the database is in WAL mode and exactly one of -wal and -shm exists. A writer creates both and
// its last close removes both, so this is an odd state (a crash mid-close, a stray copy): opening it would make
// SQLite create the missing sidecar in the owner's directory, so Open refuses and the caller keeps its previous
// values for this cycle. A WAL database with neither sidecar is not ErrIdle: it is read immutable (see the
// package doc). It is not a failure and carries no path.
var ErrIdle = errors.New("sqlro: WAL database with only one of -wal and -shm")

// DB is one read-only connection bound to an allow-list.
type DB struct {
	db    *sql.DB
	allow Allow
}

// fileID identifies a file across two path walks.
type fileID struct{ dev, ino uint64 }

// Open opens the SQLite database at path read-only, bound to allow. The checks it makes before and after SQLite
// opens the path are described in the package doc.
func Open(path string, allow Allow) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, core.Fail(core.ReasonOther)
	}
	id, immutable, err := inspect(abs)
	if err != nil {
		return nil, err
	}
	mode := "?mode=ro&"
	if immutable {
		mode = "?mode=ro&immutable=1&"
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: abs}).EscapedPath()+mode+pragmas)
	if err != nil {
		return nil, core.Fail(core.ReasonOther)
	}
	db.SetMaxOpenConns(1)
	// Reading the schema version opens the file and reads its header, so a lock, a permission problem or a file
	// that is not a database is reported here rather than by the first query.
	var v int64
	if err := db.QueryRow("PRAGMA schema_version").Scan(&v); err != nil {
		db.Close()
		return nil, reason(err)
	}
	// SQLite walked the path itself: require that it still leads, without links, to the file checked above.
	if err := sameFile(abs, id); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{db: db, allow: allow}, nil
}

// Close releases the connection.
func (d *DB) Close() error {
	if err := d.db.Close(); err != nil {
		return reason(err)
	}
	return nil
}

// inspect checks abs and its sidecars before SQLite runs and decides how to open it: it returns the database's
// identity, whether to open it immutable, or the failure (ErrIdle included).
func inspect(abs string) (fileID, bool, error) {
	dir, err := safefs.OpenDir(filepath.Dir(abs))
	if err != nil {
		return fileID{}, false, err
	}
	defer dir.Close()
	base := filepath.Base(abs)
	f, st, err := openAt(dir, base)
	if err != nil {
		return fileID{}, false, safefs.Reason(err)
	}
	defer f.Close()
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return fileID{}, false, core.Fail(core.ReasonPermission)
	}
	id := fileID{uint64(st.Dev), st.Ino}

	// SQLite opens a -wal whatever the header says, and creates -shm next to it, so both are checked for every
	// database: absent, or a regular file with a single link.
	present := 0
	for _, s := range []string{"-wal", "-shm"} {
		sf, sst, err := openAt(dir, base+s)
		if errors.Is(err, syscall.ENOENT) {
			continue
		}
		if err != nil { // ELOOP: a link; anything else is refused as well
			return fileID{}, false, core.Fail(core.ReasonPermission)
		}
		sf.Close()
		if sst.Mode&syscall.S_IFMT != syscall.S_IFREG || sst.Nlink != 1 {
			return fileID{}, false, core.Fail(core.ReasonPermission)
		}
		present++
	}

	var h [20]byte
	if n, _ := f.ReadAt(h[:], 0); n < len(h) || h[18] != 2 || h[19] != 2 {
		return id, false, nil // rollback-journal database (or too short to be one)
	}
	switch present {
	case 2:
		return id, false, nil // a writer has it open
	case 0:
		return id, true, nil // no writer: everything is in the file
	default:
		return fileID{}, false, ErrIdle
	}
}

// sameFile walks abs again without following links and requires the file checked before SQLite opened it.
func sameFile(abs string, want fileID) error {
	dir, err := safefs.OpenDir(filepath.Dir(abs))
	if err != nil {
		return err
	}
	defer dir.Close()
	f, st, err := openAt(dir, filepath.Base(abs))
	if err != nil {
		return safefs.Reason(err)
	}
	f.Close()
	if (fileID{uint64(st.Dev), st.Ino}) != want {
		return core.Fail(core.ReasonPermission)
	}
	return nil
}

// openAt opens name in dir read-only without following a link (a link fails with ELOOP) and without blocking on
// a FIFO, and returns its metadata. Errors are the raw errno, for the caller to map.
func openAt(dir *os.File, name string) (*os.File, *syscall.Stat_t, error) {
	fd, err := syscall.Openat(int(dir.Fd()), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, &st, nil
}

// fsReason maps a file system error to a contract reason without its text (which can carry a path).
func fsReason(err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return core.Fail(core.ReasonNotFound)
	case errors.Is(err, os.ErrPermission):
		return core.Fail(core.ReasonPermission)
	default:
		return core.Fail(core.ReasonOther)
	}
}

// reason maps a driver error to a contract reason without its text (which can carry SQL or a path).
func reason(err error) error {
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff { // primary result code
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return core.Fail(core.ReasonTimeout)
		case sqlite3.SQLITE_CANTOPEN, sqlite3.SQLITE_PERM, sqlite3.SQLITE_READONLY, sqlite3.SQLITE_AUTH:
			return core.Fail(core.ReasonPermission)
		}
	}
	return fsReason(err)
}
