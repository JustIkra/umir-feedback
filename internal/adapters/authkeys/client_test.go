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
	"sync"
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
	host := now
	client, err := NewClient(Options{URL: "http://auth/internal/auth/v1/jwks", ServiceKey: "key", Now: func() time.Time { return now }, RetryNow: func() time.Time { return host }, HTTP: d, AllowPlainHTTP: true})
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
	now = now.Add(DefaultMaxStale)
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
	// Within the stale window a failed background refresh still serves the cached key.
	business = business.Add(30 * time.Second)
	d.fail = true
	if _, err = c.Key(context.Background(), "one"); err != nil {
		t.Fatal("cached key within window rejected", err)
	}
	waitIdle(t, c)
	if d.calls != 3 {
		t.Fatal("stale key was not refreshed in background", d.calls)
	}
	d.fail = false
	host = host.Add(time.Second)
	if _, err = c.Key(context.Background(), "one"); err != nil {
		t.Fatal("stale failure not recovered", err)
	}
	waitIdle(t, c)
	if d.calls != 4 {
		t.Fatal("stale failure not recovered", d.calls)
	}
	business = business.Add(-time.Second)
	host = host.Add(time.Second)
	if _, err = c.Key(context.Background(), "one"); err != nil || d.calls != 5 {
		t.Fatal("owner rollback did not invalidate", err, d.calls)
	}
	business = business.Add(30 * time.Second)
	d.fail = true
	if _, err = c.Key(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, c)
	if d.calls != 6 {
		t.Fatal("failed stale refresh", d.calls)
	}
	d.fail = false
	host = host.Add(-time.Hour)
	if _, err = c.Key(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, c)
	if d.calls != 6 {
		t.Fatal("retry rollback did not restart throttle", d.calls)
	}
	host = host.Add(time.Second)
	if _, err = c.Key(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, c)
	if d.calls != 7 {
		t.Fatal("retry rollback recovery", d.calls)
	}
}

func waitIdle(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.RLock()
		idle := c.flight == nil
		c.mu.RUnlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("refresh did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

type syncDoer struct {
	mu      sync.Mutex
	calls   int
	payload string
	fail    bool
	block   chan struct{}
}

func (d *syncDoer) Do(r *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.calls++
	fail, block, payload := d.fail, d.block, d.payload
	d.mu.Unlock()
	if r.URL.Path != "/internal/auth/v1/jwks" || r.Header.Get("X-Umir-Service-Key") != "key" {
		return nil, fmt.Errorf("wrong auth request")
	}
	if block != nil {
		select {
		case <-block:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	if fail {
		return nil, fmt.Errorf("unavailable")
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(payload))}, nil
}
func (d *syncDoer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}
func (d *syncDoer) set(fail bool, block chan struct{}) {
	d.mu.Lock()
	d.fail, d.block = fail, block
	d.mu.Unlock()
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type window struct {
	client         *Client
	doer           *syncDoer
	business, host *testClock
}

func newWindow(t *testing.T, maxStale time.Duration) *window {
	t.Helper()
	key := make(ed25519.PublicKey, 32)
	key[0] = 7
	w := &window{
		doer:     &syncDoer{payload: `{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"one","x":"` + base64.RawURLEncoding.EncodeToString(key) + `","alg":"EdDSA","use":"sig"}]}`},
		business: &testClock{t: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)},
		host:     &testClock{t: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)},
	}
	c, err := NewClient(Options{URL: "http://auth/internal/auth/v1/jwks", ServiceKey: "key", Now: w.business.Now, RetryNow: w.host.Now, HTTP: w.doer, AllowPlainHTTP: true, MaxStale: maxStale})
	if err != nil {
		t.Fatal(err)
	}
	w.client = c
	return w
}

func (w *window) warm(t *testing.T) {
	t.Helper()
	if _, err := w.client.Key(context.Background(), "one"); err != nil || w.doer.count() != 1 {
		t.Fatalf("warm %v %d", err, w.doer.count())
	}
}

func TestW1ParallelCallersShareOneNetworkCall(t *testing.T) {
	w := newWindow(t, 0)
	block := make(chan struct{})
	w.doer.set(false, block)
	errs := make([]error, 10)
	var wg sync.WaitGroup
	started := time.Now()
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, errs[i] = w.client.Key(ctx, "one")
		}()
	}
	wg.Wait()
	if time.Since(started) > 3*time.Second {
		t.Fatal("callers waited past their context")
	}
	for _, err := range errs {
		if !errors.Is(err, application.ErrAuthUnavailable) {
			t.Fatalf("waiter error %v", err)
		}
	}
	if w.doer.count() != 1 {
		t.Fatalf("network calls %d", w.doer.count())
	}
	// Cancelled waiters must not cancel the shared call: it is still pending and completes for later callers.
	close(block)
	waitIdle(t, w.client)
	if _, err := w.client.Key(context.Background(), "one"); err != nil || w.doer.count() != 1 {
		t.Fatalf("shared call result lost %v %d", err, w.doer.count())
	}
}

func TestW1KnownKeyIsServedWhileRefreshHangs(t *testing.T) {
	w := newWindow(t, 0)
	w.warm(t)
	block := make(chan struct{})
	w.doer.set(false, block)
	w.business.Add(31 * time.Second)
	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		started := time.Now()
		got, err := w.client.Key(ctx, "one")
		cancel()
		if err != nil || got[0] != 7 || time.Since(started) > time.Second {
			t.Fatalf("cached key blocked %v %v", err, time.Since(started))
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for w.doer.count() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("background refresh did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if w.doer.count() != 2 {
		t.Fatalf("background refresh not shared %d", w.doer.count())
	}
	close(block)
	waitIdle(t, w.client)
	w.business.Add(31 * time.Second)
	if _, err := w.client.Key(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, w.client)
	if w.doer.count() != 3 {
		t.Fatalf("calls %d", w.doer.count())
	}
}

func TestW1AuthFailureWithinWindowServesKnownKeyOnly(t *testing.T) {
	w := newWindow(t, 0)
	w.warm(t)
	w.doer.set(true, nil)
	w.business.Add(time.Minute)
	w.host.Add(time.Second)
	if got, err := w.client.Key(context.Background(), "one"); err != nil || got[0] != 7 {
		t.Fatalf("known key within window %v", err)
	}
	waitIdle(t, w.client)
	w.host.Add(time.Second)
	if _, err := w.client.Key(context.Background(), "other"); !errors.Is(err, application.ErrAuthUnavailable) {
		t.Fatalf("unknown kid while Auth is down %v", err)
	}
	if _, err := w.client.Key(context.Background(), "other"); !errors.Is(err, application.ErrAuthUnavailable) {
		t.Fatalf("repeat unknown kid while Auth is down %v", err)
	}
}

func TestW1KnownKeyRejectedAfterStaleWindow(t *testing.T) {
	w := newWindow(t, 0)
	w.warm(t)
	w.doer.set(true, nil)
	w.business.Add(DefaultMaxStale - time.Second)
	w.host.Add(time.Second)
	if _, err := w.client.Key(context.Background(), "one"); err != nil {
		t.Fatalf("key just inside window %v", err)
	}
	waitIdle(t, w.client)
	w.business.Add(time.Second)
	w.host.Add(time.Second)
	if _, err := w.client.Key(context.Background(), "one"); !errors.Is(err, application.ErrAuthUnavailable) {
		t.Fatalf("key accepted after window %v", err)
	}
	// Auth recovers: the key is accepted again after one successful refresh.
	w.doer.set(false, nil)
	w.host.Add(time.Second)
	if _, err := w.client.Key(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
}

func TestW1ConfiguredWindowIsHonoured(t *testing.T) {
	w := newWindow(t, MinMaxStale)
	w.warm(t)
	w.doer.set(true, nil)
	w.business.Add(MinMaxStale)
	w.host.Add(time.Second)
	if _, err := w.client.Key(context.Background(), "one"); !errors.Is(err, application.ErrAuthUnavailable) {
		t.Fatalf("minimum window not applied %v", err)
	}
}

func TestW1UnknownKidRefetchIsSharedAndRateLimited(t *testing.T) {
	w := newWindow(t, 0)
	w.warm(t)
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := w.client.Key(context.Background(), "other"); !errors.Is(err, application.ErrInvalidToken) {
				t.Errorf("unknown %v", err)
			}
		}()
	}
	wg.Wait()
	if w.doer.count() != 2 {
		t.Fatalf("parallel unknown kid refetches %d", w.doer.count())
	}
	w.host.Add(900 * time.Millisecond)
	if _, err := w.client.Key(context.Background(), "another"); !errors.Is(err, application.ErrInvalidToken) || w.doer.count() != 2 {
		t.Fatalf("unknown kid refetched within a second %v %d", err, w.doer.count())
	}
	w.host.Add(100 * time.Millisecond)
	if _, err := w.client.Key(context.Background(), "another"); !errors.Is(err, application.ErrInvalidToken) || w.doer.count() != 3 {
		t.Fatalf("unknown kid not refetched after a second %v %d", err, w.doer.count())
	}
}

func TestW1HungRefreshEndsOnItsOwnDeadline(t *testing.T) {
	w := newWindow(t, 0)
	w.client.fetchTimeout = 50 * time.Millisecond
	w.doer.set(false, make(chan struct{}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := w.client.Key(ctx, "one"); !errors.Is(err, application.ErrAuthUnavailable) {
		t.Fatal(err)
	}
	waitIdle(t, w.client)
	if w.doer.count() != 1 {
		t.Fatalf("calls %d", w.doer.count())
	}
}

func TestW1MaxStaleOptionRange(t *testing.T) {
	for _, tc := range []struct {
		value time.Duration
		ok    bool
	}{{0, true}, {MinMaxStale, true}, {MaxMaxStale, true}, {MinMaxStale - time.Nanosecond, false}, {MaxMaxStale + time.Nanosecond, false}, {-time.Minute, false}} {
		_, err := NewClient(Options{URL: "http://auth/internal/auth/v1/jwks", ServiceKey: "key", Now: time.Now, HTTP: &syncDoer{}, AllowPlainHTTP: true, MaxStale: tc.value})
		if (err == nil) != tc.ok {
			t.Fatalf("%v: %v", tc.value, err)
		}
	}
}
