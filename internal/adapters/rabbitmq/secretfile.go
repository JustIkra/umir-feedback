// Source: umir-factory reference/durable@26166c8ba197

package rabbitmq

import (
	"fmt"
	"io"
)

const maxSecretFileSize = 4096

// readSecretFile reads a regular, private, small file that belongs to the
// current user (where the platform reports owners), the same checks as
// postgres.LoadDSN. The file is opened first, without following a symbolic
// link, and the checks run on the open descriptor, so the file cannot be
// swapped between the check and the read. Errors name the setting, not the path
// or the content.
func readSecretFile(path, setting string) (string, error) {
	invalid := fmt.Errorf("invalid %s: it must name a regular file readable by its owner only", setting)
	f, err := openSecretFile(path)
	if err != nil {
		return "", invalid
	}
	defer func() { ignoreErr(f.Close()) }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 ||
		info.Size() > maxSecretFileSize || !ownedByCurrentUser(info) {
		return "", invalid
	}
	content, err := io.ReadAll(io.LimitReader(f, maxSecretFileSize+1))
	if err != nil {
		return "", fmt.Errorf("cannot read %s", setting)
	}
	if len(content) > maxSecretFileSize {
		return "", invalid
	}
	return string(content), nil
}
