package delivery

import (
	"errors"
	"net/smtp"
	"strings"
)

type loginAuth struct {
	host     string
	username string
	password string
	step     uint8
}

func loginAuthFor(host, username, password string) smtp.Auth {
	return &loginAuth{host: host, username: username, password: password}
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errors.New("smtp LOGIN requires TLS")
	}
	if server.Name != a.host {
		return "", nil, errors.New("smtp LOGIN server name mismatch")
	}
	for _, mechanism := range server.Auth {
		if strings.EqualFold(mechanism, "LOGIN") {
			a.step = 1
			return "LOGIN", nil, nil
		}
	}
	return "", nil, errors.New("smtp LOGIN was not advertised")
}

func (a *loginAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		if a.step == 3 {
			a.step = 4
			return nil, nil
		}
		return nil, errors.New("unexpected smtp LOGIN completion")
	}
	if a.step == 1 && strings.EqualFold(strings.TrimSpace(string(challenge)), "username:") {
		a.step = 2
		return []byte(a.username), nil
	}
	if a.step == 2 && strings.EqualFold(strings.TrimSpace(string(challenge)), "password:") {
		a.step = 3
		return []byte(a.password), nil
	}
	return nil, errors.New("unexpected smtp LOGIN challenge")
}
