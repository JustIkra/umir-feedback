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
}
type Client struct {
	mu               sync.Mutex
	options          Options
	keys             map[string]ed25519.PublicKey
	fetchedAt        time.Time
	retryAt          time.Time
	unknownRefreshed bool
	lastError        error
}

func NewClient(options Options) (*Client, error) {
	address, err := url.Parse(options.URL)
	if err != nil || address.Host == "" || address.User != nil || address.RawQuery != "" || address.Fragment != "" || address.Path != "/internal/auth/v1/jwks" || (address.Scheme != "https" && (address.Scheme != "http" || !options.AllowPlainHTTP)) || options.ServiceKey == "" || options.Now == nil || options.HTTP == nil {
		return nil, application.ErrInvalidArgument
	}
	if options.RetryNow == nil {
		options.RetryNow = time.Now
	}
	return &Client{options: options}, nil
}
func (c *Client) Key(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	if kid == "" {
		return nil, application.ErrInvalidToken
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.options.Now()
	if now.IsZero() {
		return nil, application.ErrAuthUnavailable
	}
	if c.lastError != nil || c.fetchedAt.IsZero() || now.Before(c.fetchedAt) || now.Sub(c.fetchedAt) >= 30*time.Second {
		retryNow := c.options.RetryNow()
		if retryNow.IsZero() {
			return nil, application.ErrAuthUnavailable
		}
		if c.lastError != nil && !c.retryAt.IsZero() {
			if retryNow.Before(c.retryAt) {
				c.retryAt = retryNow
				return nil, application.ErrAuthUnavailable
			}
			if retryNow.Sub(c.retryAt) < time.Second {
				return nil, application.ErrAuthUnavailable
			}
		}
		c.retryAt = retryNow
		c.unknownRefreshed = false
		c.lastError = c.refresh(ctx)
		if c.lastError == nil {
			c.fetchedAt = now
		}
	}
	if c.lastError != nil {
		return nil, application.ErrAuthUnavailable
	}
	if key, ok := c.keys[kid]; ok {
		return append(ed25519.PublicKey(nil), key...), nil
	}
	if !c.unknownRefreshed {
		c.unknownRefreshed = true
		c.retryAt = c.options.RetryNow()
		c.lastError = c.refresh(ctx)
		if c.lastError != nil {
			return nil, application.ErrAuthUnavailable
		}
		c.fetchedAt = now
		if key, ok := c.keys[kid]; ok {
			return append(ed25519.PublicKey(nil), key...), nil
		}
	}
	return nil, application.ErrInvalidToken
}

type jwk struct {
	Type      string `json:"kty"`
	Curve     string `json:"crv"`
	ID        string `json:"kid"`
	X         string `json:"x"`
	Algorithm string `json:"alg"`
	Use       string `json:"use"`
}

func (c *Client) refresh(ctx context.Context) (err error) {
	request, err := http.NewRequestWithContext(ctx, "GET", c.options.URL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("X-Umir-Service-Key", c.options.ServiceKey)
	response, err := c.options.HTTP.Do(request)
	if err != nil {
		return err
	}
	if response == nil || response.Body == nil {
		return application.ErrAuthUnavailable
	}
	defer func() { err = errors.Join(err, response.Body.Close()) }()
	if response.StatusCode != 200 {
		return application.ErrAuthUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return application.ErrAuthUnavailable
	}
	var document struct {
		Keys []jwk `json:"keys"`
	}
	if !validKeyDocument(data) {
		return application.ErrAuthUnavailable
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&document) != nil || len(document.Keys) < 1 || len(document.Keys) > 3 {
		return application.ErrAuthUnavailable
	}
	next := make(map[string]ed25519.PublicKey, len(document.Keys))
	for _, entry := range document.Keys {
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(entry.X)
		if err != nil || len(decoded) != 32 || entry.Type != "OKP" || entry.Curve != "Ed25519" || entry.ID == "" || entry.Algorithm != "EdDSA" || entry.Use != "sig" {
			return application.ErrAuthUnavailable
		}
		if _, exists := next[entry.ID]; exists {
			return application.ErrAuthUnavailable
		}
		next[entry.ID] = ed25519.PublicKey(decoded)
	}
	c.keys = next
	return nil
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
