// Package safefs opens agent state without following links. It is shared by every plugin that reads files (the
// code moved here unchanged from internal/claude in v0.2).
package safefs

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

// Reason maps an open error to a contract reason without its text (which can carry a path).
func Reason(err error) error {
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

// OpenDir opens an absolute directory path without following any link on the way.
func OpenDir(path string) (*os.File, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return nil, core.Fail(core.ReasonOther)
	}
	fd, err := syscall.Open("/", dirFlags, 0)
	if err != nil {
		return nil, Reason(err)
	}
	for _, c := range strings.Split(path, "/") {
		if c == "" {
			continue
		}
		next, err := syscall.Openat(fd, c, dirFlags, 0)
		syscall.Close(fd)
		if err != nil {
			return nil, Reason(err)
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}

// OpenSubdir opens one child directory of an open directory, without following a link.
func OpenSubdir(dir *os.File, name string) (*os.File, error) {
	fd, err := syscall.Openat(int(dir.Fd()), name, dirFlags, 0)
	if err != nil {
		return nil, Reason(err)
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name)), nil
}

// StatAt returns size and inode of a regular file in an open directory (no link followed); ok is false for
// anything else.
func StatAt(dir *os.File, name string) (size int64, ino uint64, ok bool) {
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

// RealDir requires path to be a directory reachable without following any link.
func RealDir(path string) error {
	f, err := OpenDir(path)
	if err != nil {
		return err
	}
	return f.Close()
}

// OpenRegular opens a file read-only: no link followed on any path component (safefs.go), no blocking on FIFOs
// or devices, and anything but a regular file is refused.
func OpenRegular(path string) (*os.File, os.FileInfo, error) {
	dir, err := OpenDir(filepath.Dir(path))
	if err != nil {
		return nil, nil, err
	}
	fd, err := syscall.Openat(int(dir.Fd()), filepath.Base(path), syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	dir.Close()
	if err != nil {
		return nil, nil, Reason(err)
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
