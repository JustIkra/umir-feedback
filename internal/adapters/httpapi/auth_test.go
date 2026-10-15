package httpapi

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/application"
)

type staticKeys struct{ key ed25519.PublicKey }

func (s staticKeys) Key(context.Context, string) (ed25519.PublicKey, error) { return s.key, nil }

// TestJWTIssuedAtToleranceIsZero pins R12: Feedback accepts no clock skew, so a
// token issued even one second after the service clock is rejected.
func TestJWTIssuedAtToleranceIsZero(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	verifier, err := NewJWTVerifier(JWTOptions{Now: func() time.Time { return now }, Keys: staticKeys{public}})
	if err != nil {
		t.Fatal(err)
	}
	sign := func(iat, exp int64) string {
		encode := func(v any) string {
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			return base64.RawURLEncoding.EncodeToString(b)
		}
		head := encode(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": "k1"})
		body := encode(map[string]any{"iss": "urn:umir:auth", "aud": []string{"umir-feedback"}, "iat": iat, "nbf": iat, "exp": exp, "jti": "11111111-1111-4111-8111-111111111111", "azp": "umir-gateway", "operation": "feedback.anonymous.submit", "permissions": []string{"feedback.anonymous.submit"}, "kind": "anonymous", "sub": "service:gateway"})
		return head + "." + body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(head+"."+body)))
	}
	at := now.Unix()
	cases := []struct {
		name   string
		iat    int64
		exp    int64
		accept bool
	}{
		{"iat equals now", at, at + 60, true},
		{"iat one second before now", at - 1, at + 59, true},
		{"iat one second after now", at + 1, at + 61, false},
		{"exp is not iat plus 60", at, at + 61, false},
		{"exp equals now", at - 60, at, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verifier.Validate(context.Background(), sign(tc.iat, tc.exp), "feedback.anonymous.submit")
			if tc.accept && err != nil {
				t.Fatalf("token rejected: %v", err)
			}
			if !tc.accept && !errors.Is(err, application.ErrInvalidToken) {
				t.Fatalf("token not rejected as invalid: %v", err)
			}
		})
	}
}
