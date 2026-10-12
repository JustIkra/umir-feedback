package bootstrap

import (
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

func Healthcheck(getenv func(string) string) (returnErr error) {
	address := getenv("FEEDBACK_LISTEN_ADDRESS")
	if address == "" {
		address = "127.0.0.1:8084"
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("invalid FEEDBACK_LISTEN_ADDRESS")
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return errors.New("process health unavailable")
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			returnErr = errors.Join(returnErr, errors.New("health response close failed"))
		}
	}()
	if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 256)); err != nil {
		return errors.New("health response unavailable")
	}
	if response.StatusCode != 200 {
		return errors.New("process unhealthy")
	}
	return nil
}
