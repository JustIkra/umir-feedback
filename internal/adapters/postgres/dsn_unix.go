// Source: umir-factory reference/durable@26166c8ba197

//go:build unix

package postgres

import (
	"os"
	"syscall"
)

// openSecretFile opens path read-only without following a symbolic link in the
// last element. O_NONBLOCK keeps the open of a FIFO from waiting for a writer;
// regular files ignore it, and anything else is rejected after Stat.
func openSecretFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// ownedByCurrentUser reports whether the file belongs to the effective user.
func ownedByCurrentUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	return int(st.Uid) == os.Geteuid()
}
