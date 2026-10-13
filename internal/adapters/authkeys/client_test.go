package authkeys

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/application"
)

type testDoer struct {
	calls   int
	payload string
	fail    bool
}

func (d *testDoer) Do(r *http.Request) (*http.Response, error) {
	d.calls++
	if r.URL.Path != "/internal/auth/v1/jwks" || r.Header.Get("X-Umir-Service-Key") != "key" {
		return nil, fmt.Errorf("wrong auth request")
	}
	if d.fail {
		return nil, fmt.Errorf("unavailable")
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(d.payload))}, nil
}
func TestW1KeysBoundedRefreshAndFailClosed(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.FixedZone("MSK", 10800))
	key := make(ed25519.PublicKey, 32)
	key[0] = 7
	payload := `{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"one","x":"` + base64.RawURLEncoding.EncodeToString(key) + `","alg":"EdDSA","use":"sig"}]}`
	d := &testDoer{payload: payload}
	client, err := NewClient(Options{URL: "http://auth/internal/auth/v1/jwks", ServiceKey: "key", Now: func() time.Time { return now }, HTTP: d, AllowPlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Key(context.Background(), "one")
	if err != nil || got[0] != 7 {
		t.Fatalf("key %v %v", got, err)
	}
	got[0] = 9
	got, err = client.Key(context.Background(), "one")
	if err != nil || got[0] != 7 || d.calls != 1 {
		t.Fatalf("cache copy %v %v %d", got, err, d.calls)
	}
	for range 10 {
		if _, err := client.Key(context.Background(), "unknown"); !errors.Is(err, application.ErrInvalidToken) {
			t.Fatalf("unknown %v", err)
		}
	}
	if d.calls != 2 {
		t.Fatalf("unbounded unknown refresh %d", d.calls)
	}
	now = now.Add(30 * time.Second)
	d.fail = true
	if _, err := client.Key(context.Background(), "one"); !errors.Is(err, application.ErrAuthUnavailable) {
		t.Fatalf("stale discovery %v", err)
	}
}
func TestW1KeysRejectMalformedDiscovery(t *testing.T) {
	for _, payload := range []string{`{"keys":[]}`, `{"keys":[],"extra":true}`, `{"keys":null}`, `{"keys":[{"kty":"RSA","crv":"Ed25519","kid":"one","x":"bad"}]}`, `{"keys":[],"keys":[]}`, `{"KEYS":[{"kty":"OKP","crv":"Ed25519","kid":"one","x":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","alg":"EdDSA","use":"sig"}]}`, `{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"one","x":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","alg":"EdDSA","use":"sig"}]} {"keys":[]}`} {
		t.Run(payload, func(t *testing.T) {
			d := &testDoer{payload: payload}
			client, err := NewClient(Options{URL: "http://auth/internal/auth/v1/jwks", ServiceKey: "key", Now: time.Now, HTTP: d, AllowPlainHTTP: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Key(context.Background(), "one"); err == nil {
				t.Fatal("malformed keys accepted")
			}
		})
	}
}

func TestW6RetryTimeDoesNotControlKeyValidity(t *testing.T) {
	business := time.Date(2026, 10, 13, 12, 0, 0, 0, time.FixedZone("MSK", 10800))
	host := business
	key := make(ed25519.PublicKey, 32)
	d := &testDoer{fail: true, payload: `{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"one","x":"` + base64.RawURLEncoding.EncodeToString(key) + `","alg":"EdDSA","use":"sig"}]}`}
	c, err := NewClient(Options{URL: "http://auth/internal/auth/v1/jwks", ServiceKey: "key", Now: func() time.Time { return business }, RetryNow: func() time.Time { return host }, HTTP: d, AllowPlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if _, err = c.Key(context.Background(), "one"); !errors.Is(err, application.ErrAuthUnavailable) {
			t.Fatal(err)
		}
	}
	if d.calls != 1 {
		t.Fatal("failed discovery not throttled", d.calls)
	}
	d.fail = false
	host = host.Add(time.Second)
	if _, err = c.Key(context.Background(), "one"); err != nil || d.calls != 2 {
		t.Fatal("fixed business clock could not recover", err, d.calls)
	}
	host = host.Add(time.Hour)
	if _, err = c.Key(context.Background(), "one"); err != nil || d.calls != 2 {
		t.Fatal("host time changed cache validity", err, d.calls)
	}
	business = business.Add(30 * time.Second)
	d.fail = true
	if _, err = c.Key(context.Background(), "one"); !errors.Is(err, application.ErrAuthUnavailable) || d.calls != 3 {
		t.Fatal("stale key accepted", err, d.calls)
	}
	d.fail = false
	host = host.Add(time.Second)
	if _, err = c.Key(context.Background(), "one"); err != nil || d.calls != 4 {
		t.Fatal("stale failure not recovered", err, d.calls)
	}
	business = business.Add(-time.Second)
	host = host.Add(time.Second)
	if _, err = c.Key(context.Background(), "one"); err != nil || d.calls != 5 {
		t.Fatal("owner rollback did not invalidate", err, d.calls)
	}
	business = business.Add(30 * time.Second)
	d.fail = true
	if _, err = c.Key(context.Background(), "one"); !errors.Is(err, application.ErrAuthUnavailable) || d.calls != 6 {
		t.Fatal("failed stale refresh", err, d.calls)
	}
	d.fail = false
	host = host.Add(-time.Hour)
	if _, err = c.Key(context.Background(), "one"); !errors.Is(err, application.ErrAuthUnavailable) || d.calls != 6 {
		t.Fatal("retry rollback did not restart throttle", err, d.calls)
	}
	host = host.Add(time.Second)
	if _, err = c.Key(context.Background(), "one"); err != nil || d.calls != 7 {
		t.Fatal("retry rollback recovery", err, d.calls)
	}
}
