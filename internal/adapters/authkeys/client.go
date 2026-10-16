package authkeys

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/JustIkra/umir-feedback/internal/application"
)

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}
type Options struct {
	URL, ServiceKey string
	Now             func() time.Time
	RetryNow        func() time.Time
	HTTP            HTTPClient
	AllowPlainHTTP  bool
	// MaxStale is how long a snapshot that could not be refreshed stays usable.
	// Zero means DefaultMaxStale; other values must lie in [MinMaxStale, MaxMaxStale].
	MaxStale time.Duration
}

// The Auth service signs 60-second tokens and keeps the previous signing key in
// JWKS for at least 60 seconds after its last use. A snapshot that is up to
// MaxStale old therefore still contains every key that can sign a token that is
// valid right now: the stale window never extends the lifetime of a token, it
// only lets already issued tokens be verified while Auth is briefly unreachable.
// The window keeps nothing but the key: signature, aud, azp and exp are always
// checked by the verifier. A snapshot older than the window is rejected.
const (
	DefaultMaxStale = 5 * time.Minute
	MinMaxStale     = 30 * time.Second
	MaxMaxStale     = 10 * time.Minute

	// refreshAfter is the age after which a use of the snapshot also starts a
	// background refresh. The caller is not delayed by it.
	refreshAfter = 30 * time.Second
	// retryInterval limits refetches after a failure and refetches caused by an unknown kid.
	retryInterval = time.Second
	// fetchTimeout bounds the one shared network call. It has its own context, so
	// a caller that gives up never cancels the call for the others, and the
	// goroutine that runs it always ends.
	fetchTimeout = 10 * time.Second
)

// flightCall is the single shared network call. err is set before done is closed.
type flightCall struct {
	done chan struct{}
	err  error
	now  time.Time
}

type Client struct {
	mu           sync.RWMutex
	options      Options
	maxStale     time.Duration
	fetchTimeout time.Duration
	keys         map[string]ed25519.PublicKey // replaced as a whole, never modified in place
	fetchedAt    time.Time                    // business clock, start of the last successful fetch
	retryAt      time.Time                    // host clock, start of the last fetch
	unknownAt    time.Time                    // host clock, last refetch caused by an unknown kid
	lastFailed   bool
	flight       *flightCall
}

func NewClient(options Options) (*Client, error) {
	address, err := url.Parse(options.URL)
	if err != nil || address.Host == "" || address.User != nil || address.RawQuery != "" || address.Fragment != "" || address.Path != "/internal/auth/v1/jwks" || (address.Scheme != "https" && (address.Scheme != "http" || !options.AllowPlainHTTP)) || options.ServiceKey == "" || options.Now == nil || options.HTTP == nil {
		return nil, application.ErrInvalidArgument
	}
	if options.RetryNow == nil {
		options.RetryNow = time.Now
	}
	maxStale := options.MaxStale
	if maxStale == 0 {
		maxStale = DefaultMaxStale
	}
	if maxStale < MinMaxStale || maxStale > MaxMaxStale {
		return nil, application.ErrInvalidArgument
	}
	return &Client{options: options, maxStale: maxStale, fetchTimeout: fetchTimeout}, nil
}

func (c *Client) Key(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	if kid == "" {
		return nil, application.ErrInvalidToken
	}
	now := c.options.Now()
	if now.IsZero() {
		return nil, application.ErrAuthUnavailable
	}
	c.mu.RLock()
	keys, fetchedAt := c.keys, c.fetchedAt
	c.mu.RUnlock()
	if c.usable(now, fetchedAt) {
		if key, ok := keys[kid]; ok {
			if now.Sub(fetchedAt) >= refreshAfter {
				c.refreshInBackground(now)
			}
			return append(ed25519.PublicKey(nil), key...), nil
		}
	}
	return c.lookupAfterRefresh(ctx, kid, now)
}

// usable reports whether the snapshot may answer now: it exists, the owner clock
// did not move back, and it is younger than the stale window.
func (c *Client) usable(now, fetchedAt time.Time) bool {
	return !fetchedAt.IsZero() && !now.Before(fetchedAt) && now.Sub(fetchedAt) < c.maxStale
}

// refreshInBackground starts a refresh when none is running and the failure
// throttle allows it. It never waits.
func (c *Client) refreshInBackground(now time.Time) {
	retryNow := c.options.RetryNow()
	if retryNow.IsZero() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.flight != nil || !c.failureGate(retryNow) {
		return
	}
	c.startFlight(now, retryNow)
}

// lookupAfterRefresh is the slow path: there is no usable snapshot, or the kid is
// not in it. It joins the shared call, or starts it if the throttles allow.
func (c *Client) lookupAfterRefresh(ctx context.Context, kid string, now time.Time) (ed25519.PublicKey, error) {
	retryNow := c.options.RetryNow()
	if retryNow.IsZero() {
		return nil, application.ErrAuthUnavailable
	}
	c.mu.Lock()
	usable := c.usable(now, c.fetchedAt)
	if usable {
		if key, ok := c.keys[kid]; ok {
			c.mu.Unlock()
			return append(ed25519.PublicKey(nil), key...), nil
		}
	}
	flight := c.flight
	if flight == nil {
		if !c.failureGate(retryNow) {
			c.mu.Unlock()
			return nil, application.ErrAuthUnavailable
		}
		if usable {
			// An unknown kid: at most one refetch per second, shared by all callers.
			if !c.unknownGate(retryNow) {
				failed := c.lastFailed
				c.mu.Unlock()
				if failed {
					return nil, application.ErrAuthUnavailable
				}
				return nil, application.ErrInvalidToken
			}
			c.unknownAt = retryNow
		}
		flight = c.startFlight(now, retryNow)
	}
	c.mu.Unlock()
	select {
	case <-flight.done:
	case <-ctx.Done():
		return nil, application.ErrAuthUnavailable
	}
	if flight.err != nil {
		return nil, application.ErrAuthUnavailable
	}
	c.mu.RLock()
	key, ok := c.keys[kid]
	c.mu.RUnlock()
	if !ok {
		return nil, application.ErrInvalidToken
	}
	return append(ed25519.PublicKey(nil), key...), nil
}

// failureGate lets a fetch start at most once per second after a failure. A host
// clock that moved back restarts the throttle. The caller holds c.mu.
func (c *Client) failureGate(retryNow time.Time) bool {
	if !c.lastFailed || c.retryAt.IsZero() {
		return true
	}
	if retryNow.Before(c.retryAt) {
		c.retryAt = retryNow
		return false
	}
	return retryNow.Sub(c.retryAt) >= retryInterval
}

// unknownGate lets an unknown kid cause one refetch per second. The caller holds c.mu.
func (c *Client) unknownGate(retryNow time.Time) bool {
	if c.unknownAt.IsZero() {
		return true
	}
	if retryNow.Before(c.unknownAt) {
		c.unknownAt = retryNow
		return false
	}
	return retryNow.Sub(c.unknownAt) >= retryInterval
}

// startFlight starts the shared network call. The caller holds c.mu and has
// checked that no call is running. The goroutine owns its context, ends when the
// call ends or after fetchTimeout, and reads no clock: the start times are passed in.
func (c *Client) startFlight(now, retryNow time.Time) *flightCall {
	flight := &flightCall{done: make(chan struct{}), now: now}
	c.flight = flight
	c.retryAt = retryNow
	timeout := c.fetchTimeout
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		next, err := c.refresh(ctx)
		c.mu.Lock()
		if err == nil {
			c.keys, c.fetchedAt = next, flight.now
		}
		c.lastFailed = err != nil
		c.flight = nil
		c.mu.Unlock()
		flight.err = err
		close(flight.done)
	}()
	return flight
}

type jwk struct {
	Type      string `json:"kty"`
	Curve     string `json:"crv"`
	ID        string `json:"kid"`
	X         string `json:"x"`
	Algorithm string `json:"alg"`
	Use       string `json:"use"`
}

func (c *Client) refresh(ctx context.Context) (keys map[string]ed25519.PublicKey, err error) {
	request, err := http.NewRequestWithContext(ctx, "GET", c.options.URL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-Umir-Service-Key", c.options.ServiceKey)
	response, err := c.options.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	if response == nil || response.Body == nil {
		return nil, application.ErrAuthUnavailable
	}
	defer func() { err = errors.Join(err, response.Body.Close()) }()
	if response.StatusCode != 200 {
		return nil, application.ErrAuthUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return nil, application.ErrAuthUnavailable
	}
	var document struct {
		Keys []jwk `json:"keys"`
	}
	if !validKeyDocument(data) {
		return nil, application.ErrAuthUnavailable
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&document) != nil || len(document.Keys) < 1 || len(document.Keys) > 3 {
		return nil, application.ErrAuthUnavailable
	}
	next := make(map[string]ed25519.PublicKey, len(document.Keys))
	for _, entry := range document.Keys {
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(entry.X)
		if err != nil || len(decoded) != 32 || entry.Type != "OKP" || entry.Curve != "Ed25519" || entry.ID == "" || entry.Algorithm != "EdDSA" || entry.Use != "sig" {
			return nil, application.ErrAuthUnavailable
		}
		if _, exists := next[entry.ID]; exists {
			return nil, application.ErrAuthUnavailable
		}
		next[entry.ID] = ed25519.PublicKey(decoded)
	}
	return next, nil
}
func validKeyDocument(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if scanJSON(decoder) != nil {
		return false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return false
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(data, &document) != nil || len(document) != 1 {
		return false
	}
	raw, ok := document["keys"]
	if !ok {
		return false
	}
	var keys []map[string]json.RawMessage
	if json.Unmarshal(raw, &keys) != nil {
		return false
	}
	for _, key := range keys {
		if len(key) != 6 {
			return false
		}
		for _, name := range []string{"kty", "crv", "kid", "x", "alg", "use"} {
			if _, ok := key[name]; !ok {
				return false
			}
		}
	}
	return true
}

func scanJSON(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch token {
	case nil:
		return application.ErrAuthUnavailable
	case json.Delim('{'):
		seen := make(map[string]bool)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return application.ErrAuthUnavailable
			}
			seen[name] = true
			if err := scanJSON(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case json.Delim('['):
		for decoder.More() {
			if err := scanJSON(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return nil
	}
}
