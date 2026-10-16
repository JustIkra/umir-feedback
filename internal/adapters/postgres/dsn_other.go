// Source: umir-factory reference/durable@26166c8ba197

//go:build !unix

package postgres

import "os"

// openSecretFile opens path read-only. Without O_NOFOLLOW the symbolic link
// check falls back to the mode reported for the opened file.
func openSecretFile(path string) (*os.File, error) { return os.Open(path) }

// ownedByCurrentUser cannot tell owners apart on this platform.
func ownedByCurrentUser(os.FileInfo) bool { return true }
