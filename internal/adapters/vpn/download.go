package vpn

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxSubscriptionBytes = 1 << 20

var errDownloadFailed = errors.New("VPN subscription download failed")

func DownloadSubscription(ctx context.Context, rawURL string) ([]byte, error) {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
	}
	return downloadSubscription(ctx, rawURL, client)
}

func downloadSubscription(ctx context.Context, rawURL string, client *http.Client) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, errDownloadFailed
	}
	boundedClient := *client
	boundedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, errDownloadFailed
	}
	request.Header.Set("Accept", "text/plain")
	response, err := boundedClient.Do(request)
	if err != nil {
		boundedClient.CloseIdleConnections()
		return nil, errDownloadFailed
	}
	defer boundedClient.CloseIdleConnections()
	if response.StatusCode != http.StatusOK || response.ContentLength > maxSubscriptionBytes {
		return nil, errors.Join(errDownloadFailed, response.Body.Close())
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxSubscriptionBytes+1))
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil || len(body) == 0 || len(body) > maxSubscriptionBytes || strings.IndexByte(string(body), 0) >= 0 {
		return nil, errDownloadFailed
	}
	return body, nil
}
