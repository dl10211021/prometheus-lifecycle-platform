//go:build linux

package sqlite

import (
	"golang.org/x/sys/unix"
	"os"
	"syscall"
)

func lockDatabaseProcessFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}
func regularSingleLink(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Nlink == 1
}
