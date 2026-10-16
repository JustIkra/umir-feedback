// Source: umir-factory reference/durable@26166c8ba197

package postgres

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
)

const maxSecretFileSize = 4096

var envPrefix = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// LoadDSN returns the runtime database URL of the service whose environment
// variables start with prefix (for example WAREHOUSE): <PREFIX>_DATABASE_URL or
// <PREFIX>_DATABASE_URL_FILE. Setting both is an error. The value is never
// shown in an error.
func LoadDSN(getenv func(string) string, prefix string) (string, error) {
	if !envPrefix.MatchString(prefix) {
		return "", errors.New("invalid environment prefix")
	}
	return loadDSN(getenv, prefix+"_DATABASE_URL", true)
}

// LoadMigrateDSN returns the owner-role URL for the migrate command from
// <PREFIX>_MIGRATE_DATABASE_URL_FILE (C8, C10). The owner URL does not travel in
// the environment: <PREFIX>_MIGRATE_DATABASE_URL is an error, with or without
// the file. Only the migrate job is given this file.
func LoadMigrateDSN(getenv func(string) string, prefix string) (string, error) {
	if !envPrefix.MatchString(prefix) {
		return "", errors.New("invalid environment prefix")
	}
	return loadDSN(getenv, prefix+"_MIGRATE_DATABASE_URL", false)
}

func loadDSN(getenv func(string) string, key string, allowVariable bool) (string, error) {
	value, file := getenv(key), getenv(key+"_FILE")
	switch {
	case value != "" && file != "":
		return "", fmt.Errorf("configure only %s or %s_FILE, not both", key, key)
	case value != "" && !allowVariable:
		return "", fmt.Errorf("%s is not accepted: use %s_FILE", key, key)
	case value == "" && file == "":
		if allowVariable {
			return "", fmt.Errorf("missing %s or %s_FILE", key, key)
		}
		return "", fmt.Errorf("missing %s_FILE", key)
	case file != "":
		var err error
		if value, err = readSecretFile(file, key+"_FILE"); err != nil {
			return "", err
		}
	}
	if err := validateURL(value); err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	return value, nil
}

// readSecretFile reads a regular, private, small file that belongs to the
// current user (where the platform reports owners). The file is opened first,
// without following a symbolic link, and the checks run on the open descriptor,
// so the file cannot be swapped between the check and the read. Errors name the
// setting, not the path or the content.
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
	value := strings.TrimSuffix(strings.TrimSuffix(string(content), "\n"), "\r")
	if value == "" {
		return "", fmt.Errorf("empty %s", setting)
	}
	return value, nil
}

// validateURL accepts a postgres:// URL and says nothing about its content.
func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || (u.Host == "" && u.Query().Get("host") == "") {
		return errors.New("the database URL must be a postgres:// URL")
	}
	return nil
}
