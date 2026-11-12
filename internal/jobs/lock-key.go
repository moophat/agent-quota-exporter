package jobs

import (
	"fmt"
	"os"
	"syscall"
)

// lockKeyOf is "major:minor:inode" in decimal for a listed file, the form heldLocks normalizes /proc/locks to.
func lockKeyOf(fi os.FileInfo) (string, bool) {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return lockKey(uint64(sys.Dev), sys.Ino), true
}

// lockKey decodes a Linux device number into major and minor.
func lockKey(dev, ino uint64) string {
	major := (dev>>8)&0xfff | (dev>>32)&^uint64(0xfff)
	minor := dev&0xff | (dev>>12)&^uint64(0xff)
	return fmt.Sprintf("%d:%d:%d", major, minor, ino)
}

// fileID is the device and inode of a file, to tell a replaced log from the one being read.
func fileID(fi os.FileInfo) (dev, ino uint64) {
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(sys.Dev), sys.Ino
	}
	return 0, 0
}
