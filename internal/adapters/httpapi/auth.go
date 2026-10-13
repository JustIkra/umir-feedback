package httpapi

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

type SigningKeys interface {
	Key(context.Context, string) (ed25519.PublicKey, error)
}
type JWTOptions struct {
	Now  func() time.Time
	Keys SigningKeys
}
type JWTVerifier struct{ options JWTOptions }

func NewJWTVerifier(options JWTOptions) (*JWTVerifier, error) {
	if options.Now == nil || options.Keys == nil {
		return nil, application.ErrInvalidArgument
	}
	return &JWTVerifier{options}, nil
}

type jwtHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
	KeyID     string `json:"kid"`
}
type jwtClaims struct {
	Issuer            string   `json:"iss"`
	Audiences         []string `json:"aud"`
	IssuedAt          *int64   `json:"iat"`
	NotBefore         *int64   `json:"nbf"`
	Expires           *int64   `json:"exp"`
	ID                string   `json:"jti"`
	Operation         string   `json:"operation"`
	Permissions       []string `json:"permissions"`
	Kind              string   `json:"kind"`
	Subject           string   `json:"sub"`
	SessionID         string   `json:"sid"`
	SessionVersion    string   `json:"session_version"`
	CredentialVersion string   `json:"credential_version"`
	AccessVersion     string   `json:"access_version"`
	OrganizationID    string   `json:"org_id"`
	TenantID          string   `json:"tenant_id"`
	TimeZone          string   `json:"time_zone"`
	SettingsVersion   string   `json:"settings_version"`
	LifecycleVersion  string   `json:"lifecycle_version"`
	RuntimeEpoch      string   `json:"runtime_epoch"`
	AuthorizedParty   string   `json:"azp"`
}

func decodeJWTObject(segment string, target any) error {
	data, e := base64.RawURLEncoding.Strict().DecodeString(segment)
	if e != nil || !utf8.Valid(data) || !validEscapes(data) {
		return application.ErrInvalidToken
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	if uniqueKeys(d) != nil {
		return application.ErrInvalidToken
	}
	if _, e := d.Token(); !errors.Is(e, io.EOF) {
		return application.ErrInvalidToken
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return application.ErrInvalidToken
	}
	for _, v := range fields {
		if string(v) == "null" {
			return application.ErrInvalidToken
		}
	}
	d = json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil {
		return application.ErrInvalidToken
	}
	return nil
}
func (v *JWTVerifier) Validate(ctx context.Context, token, operation string) (domain.Namespace, error) {
	fail := func() (domain.Namespace, error) { return domain.Namespace{}, application.ErrInvalidToken }
	if len(token) == 0 || len(token) > 16384 {
		return fail()
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fail()
	}
	var h jwtHeader
	var c jwtClaims
	if decodeJWTObject(parts[0], &h) != nil || h.Algorithm != "EdDSA" || h.Type != "JWT" || h.KeyID == "" || decodeJWTObject(parts[1], &c) != nil {
		return fail()
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fail()
	}
	key, err := v.options.Keys.Key(ctx, h.KeyID)
	if err != nil {
		return domain.Namespace{}, err
	}
	if len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return fail()
	}
	now := v.options.Now()
	if now.IsZero() {
		return domain.Namespace{}, application.ErrAuthUnavailable
	}
	if c.Issuer != "urn:umir:auth" || !slices.Equal(c.Audiences, []string{"umir-feedback"}) || c.AuthorizedParty != "umir-gateway" || c.IssuedAt == nil || c.NotBefore == nil || c.Expires == nil || *c.IssuedAt < 0 || *c.IssuedAt > now.Unix() || *c.NotBefore != *c.IssuedAt || *c.Expires-*c.IssuedAt != 60 || *c.Expires <= now.Unix() || !canonicalID(c.ID) {
		return fail()
	}
	required := []string{"iss", "aud", "iat", "nbf", "exp", "jti", "operation", "permissions", "kind", "sub", "azp"}
	capability := ""
	switch operation {
	case "appFeedback":
		capability = "feedback.submit"
	case "publicFeedbackRead":
		capability = "feedback.read"
	case "publicFeedbackExport":
		capability = "feedback.export"
	case "feedback.anonymous.submit":
		capability = operation
	default:
		return domain.Namespace{}, application.ErrForbidden
	}
	ns := domain.Anonymous()
	if c.Kind == "user" {
		required = append(required, "sid", "session_version", "credential_version", "access_version", "org_id", "tenant_id", "time_zone", "settings_version", "lifecycle_version")
		ns = domain.Namespace{Kind: "app", UserID: c.Subject, OrgID: c.OrganizationID, TenantID: c.TenantID}
		if !ns.Valid() || !canonicalID(c.SessionID) || !version(c.SessionVersion) || !version(c.CredentialVersion) || !version(c.AccessVersion) || !version(c.SettingsVersion) || !version(c.LifecycleVersion) || c.TimeZone == "" || c.TimeZone == "Local" || len(c.TimeZone) > 128 {
			return fail()
		}
		if _, e := time.LoadLocation(c.TimeZone); e != nil {
			return fail()
		}
	} else if c.Kind != "anonymous" || c.Subject != "service:gateway" {
		return fail()
	}
	raw, decodeErr := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if decodeErr != nil {
		return fail()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != len(required) {
		return fail()
	}
	for _, k := range required {
		if _, ok := fields[k]; !ok {
			return fail()
		}
	}
	if c.Operation != operation || !slices.Equal(c.Permissions, []string{capability}) || (c.Kind == "anonymous") != (operation == "feedback.anonymous.submit") {
		return domain.Namespace{}, application.ErrForbidden
	}
	return ns, nil
}
func canonicalID(v string) bool {
	return v == strings.ToLower(v) && domain.ValidID(domain.SubmissionID(v))
}
func version(v string) bool {
	if len(v) == 0 || len(v) > 19 || v[0] == '0' {
		return false
	}
	n, e := strconv.ParseUint(v, 10, 64)
	return e == nil && n > 0
}
