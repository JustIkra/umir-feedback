package vpn

import (
	"errors"
	"strings"
)

func parseProxyCredentials(username, password []byte) (ProxyCredentials, error) {
	trimFileNewline := func(value []byte) string { return strings.TrimSuffix(strings.TrimSuffix(string(value), "\n"), "\r") }
	credentials := ProxyCredentials{Username: trimFileNewline(username), Password: trimFileNewline(password)}
	if !validCredential(credentials.Username) || !validCredential(credentials.Password) || strings.TrimSpace(credentials.Username) == "" || strings.TrimSpace(credentials.Password) == "" || strings.Contains(credentials.Username, ":") {
		return ProxyCredentials{}, errors.New("invalid VPN proxy credentials")
	}
	return credentials, nil
}
