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
// Open refuses a path with a symlink in any component or a target that is not a regular file, then opens
// file:<path>?mode=ro (never immutable: the database is live, often in WAL mode) with query_only, a 200 ms busy
// timeout, a 256 KiB page cache, no mmap and a single connection. Callers open once per collect cycle and Close
// after, so no file handle or cache outlives the cycle. A lock held by the owning program surfaces as
// core.ReasonTimeout: the caller retries next cycle instead of waiting.
package sqlro

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// pragmas are applied to every connection (busy_timeout first, by the driver).
const pragmas = "_pragma=busy_timeout(200)&_pragma=query_only(1)&_pragma=cache_size(-256)&_pragma=mmap_size(0)"

// ErrIdle means the database is in WAL mode but its -wal or -shm is absent: no writer has it open, so nothing
// can have changed since the owner last closed it. Opening it now would make SQLite create both sidecars in the
// owner's directory (as root, chowned to the owner) and leave them behind, so Open refuses and the caller keeps
// its previous values for this cycle. It is not a failure and carries no path.
var ErrIdle = errors.New("sqlro: WAL database not open by any writer")

// DB is one read-only connection bound to an allow-list.
type DB struct {
	db    *sql.DB
	allow Allow
}

// Open opens the SQLite database at path read-only, bound to allow.
//
// The symlink check is a walk with Lstat before SQLite opens the path itself (SQLite must also open -wal and
// -shm by name, so a descriptor-based open is not possible); a link swapped in between the check and the open
// is not caught.
func Open(path string, allow Allow) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, core.Fail(core.ReasonOther)
	}
	if err := checkPath(abs); err != nil {
		return nil, err
	}
	if idle, err := walIdle(abs); err != nil {
		return nil, err
	} else if idle {
		return nil, ErrIdle
	}
	dsn := "file:" + (&url.URL{Path: abs}).EscapedPath() + "?mode=ro&" + pragmas
	db, err := sql.Open("sqlite", dsn)
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
	return &DB{db: db, allow: allow}, nil
}

// Close releases the connection.
func (d *DB) Close() error {
	if err := d.db.Close(); err != nil {
		return reason(err)
	}
	return nil
}

// walIdle reports whether the file header says WAL (bytes 18 and 19 are 2) while -wal or -shm is missing. A
// writer that closes between this check and SQLite's open can still leave fresh sidecars behind; the window is
// one open.
func walIdle(abs string) (bool, error) {
	f, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, fsReason(err)
	}
	defer f.Close()
	var h [20]byte
	if n, _ := f.ReadAt(h[:], 0); n < len(h) || h[18] != 2 || h[19] != 2 {
		return false, nil // rollback-journal database (or too short to be one): SQLite creates no sidecar
	}
	for _, s := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(abs + s); err != nil {
			return true, nil
		}
	}
	return false, nil
}

// checkPath requires every component of the absolute path to be a non-link and the target a regular file.
func checkPath(abs string) error {
	p := "/"
	var fi os.FileInfo
	for _, c := range strings.Split(abs, "/") {
		if c == "" {
			continue
		}
		p = filepath.Join(p, c)
		var err error
		if fi, err = os.Lstat(p); err != nil {
			return fsReason(err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return core.Fail(core.ReasonPermission)
		}
	}
	if fi == nil || !fi.Mode().IsRegular() {
		return core.Fail(core.ReasonPermission)
	}
	return nil
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
