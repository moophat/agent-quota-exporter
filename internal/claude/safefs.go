package claude

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// The exporter may run as root over directories their users control. Every path it reads is therefore opened
// one component at a time from "/" with O_NOFOLLOW (openat), so a link at any level, including one swapped in
// between two reads, is refused instead of followed. Profile dirs are resolved to their real path at start, so a
// legitimate profile has no link on its path.

const dirFlags = syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC

// fsReason maps an open error to a contract reason without its text (which can carry a path).
func fsReason(err error) error {
	switch {
	case errors.Is(err, syscall.ENOENT):
		return core.Fail(core.ReasonNotFound)
	case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.EACCES),
		errors.Is(err, syscall.EPERM):
		return core.Fail(core.ReasonPermission)
	default:
		return core.Fail(core.ReasonOther)
	}
}

// openDir opens an absolute directory path without following any link on the way.
func openDir(path string) (*os.File, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return nil, core.Fail(core.ReasonOther)
	}
	fd, err := syscall.Open("/", dirFlags, 0)
	if err != nil {
		return nil, fsReason(err)
	}
	for _, c := range strings.Split(path, "/") {
		if c == "" {
			continue
		}
		next, err := syscall.Openat(fd, c, dirFlags, 0)
		syscall.Close(fd)
		if err != nil {
			return nil, fsReason(err)
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}

// openSubdir opens one child directory of an open directory, without following a link.
func openSubdir(dir *os.File, name string) (*os.File, error) {
	fd, err := syscall.Openat(int(dir.Fd()), name, dirFlags, 0)
	if err != nil {
		return nil, fsReason(err)
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name)), nil
}

// statAt returns size and inode of a regular file in an open directory (no link followed); ok is false for
// anything else.
func statAt(dir *os.File, name string) (size int64, ino uint64, ok bool) {
	fd, err := syscall.Openat(int(dir.Fd()), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return 0, 0, false
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return 0, 0, false
	}
	return st.Size, st.Ino, true
}

// realDir requires path to be a directory reachable without following any link.
func realDir(path string) error {
	f, err := openDir(path)
	if err != nil {
		return err
	}
	return f.Close()
}
