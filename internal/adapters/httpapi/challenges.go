package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

type ChallengeService interface {
	Begin(context.Context, application.ChallengeBegin) (domain.ChallengeReceipt, error)
	Resend(context.Context, application.ChallengeResend) (domain.ChallengeReceipt, error)
	Verify(context.Context, application.ChallengeVerify) (domain.ChallengeProof, error)
	Complete(context.Context, application.ChallengeComplete) (domain.ChallengeCompletion, error)
}

func NewChallengeHandler(logger *slog.Logger, core ChallengeService, token string) (http.Handler, error) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(decoded) < 32 || len(decoded) > 128 || logger == nil || core == nil {
		return nil, errors.New("invalid private challenge configuration")
	}
	clear(decoded)
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		var identity [16]byte
		if _, err := rand.Read(identity[:]); err != nil {
			w.WriteHeader(503)
			logger.Error("challenge request identity unavailable")
			return
		}
		requestID := hex.EncodeToString(identity[:])
		respond := func(status int, code string) {
			writeJSON(w, status, errorBody{Code: code, Message: code, RequestID: requestID}, logger)
		}
		auth := r.Header.Values("Authorization")
		if len(auth) != 1 || !strings.HasPrefix(auth[0], "Bearer ") || len(auth[0]) > 256 {
			respond(401, "Unauthorized")
			return
		}
		got := sha256.Sum256([]byte(strings.TrimPrefix(auth[0], "Bearer ")))
		if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
			respond(401, "Unauthorized")
			return
		}
		if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.URL.EscapedPath() != r.URL.Path {
			respond(400, "InvalidRequest")
			return
		}
		const base = "/internal/v1/email-challenges"
		operation := "begin"
		var challengeID domain.ChallengeID
		if r.URL.Path != base {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, base+"/"), "/")
			if !strings.HasPrefix(r.URL.Path, base+"/") || len(parts) != 2 || !validPrivateID(parts[0]) {
				respond(404, "ChallengeUnavailable")
				return
			}
			challengeID = domain.ChallengeID(strings.ToLower(parts[0]))
			operation = parts[1]
		}
		keys := []string{"registrationID", "purpose"}
		switch operation {
		case "begin":
			keys = append(keys, "email")
		case "resend":
			keys = append(keys, "issuanceID")
		case "verify":
			keys = append(keys, "verificationID", "code")
		case "complete":
			keys = append(keys, "completionID", "proofID")
		default:
			respond(404, "ChallengeUnavailable")
			return
		}
		media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) {
			respond(400, "InvalidRequest")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			respond(400, "InvalidRequest")
			return
		}
		defer clear(body)
		fields, err := decodeChallengeFields(body, keys)
		if err != nil || !validPrivateID(fields["registrationID"]) || fields["purpose"] != string(domain.OperatorRegistration) {
			respond(400, "InvalidRequest")
			return
		}
		registration := domain.RegistrationID(strings.ToLower(fields["registrationID"]))
		purpose := domain.OperatorRegistration
		var result any
		status := 200
		switch operation {
		case "begin":
			address := strings.TrimSpace(fields["email"])
			parsed, parseErr := mail.ParseAddress(address)
			if parseErr != nil || parsed.Name != "" || parsed.Address != address || !domain.ValidEmail(domain.NormalizeEmail(address)) {
				respond(400, "InvalidRequest")
				return
			}
			result, err = core.Begin(r.Context(), application.ChallengeBegin{RegistrationID: registration, Purpose: purpose, Email: address})
			status = 202
		case "resend":
			if !validPrivateID(fields["issuanceID"]) {
				respond(400, "InvalidRequest")
				return
			}
			result, err = core.Resend(r.Context(), application.ChallengeResend{ChallengeID: challengeID, RegistrationID: registration, Purpose: purpose, IssuanceID: domain.IssuanceID(strings.ToLower(fields["issuanceID"]))})
		case "verify":
			if !validPrivateID(fields["verificationID"]) || !canonicalCode(fields["code"]) {
				respond(400, "InvalidRequest")
				return
			}
			result, err = core.Verify(r.Context(), application.ChallengeVerify{ChallengeID: challengeID, RegistrationID: registration, Purpose: purpose, VerificationID: domain.VerificationID(strings.ToLower(fields["verificationID"])), Code: fields["code"]})
			fields["code"] = ""
		case "complete":
			if !validPrivateID(fields["completionID"]) || !validProof(fields["proofID"]) {
				respond(400, "InvalidRequest")
				return
			}
			result, err = core.Complete(r.Context(), application.ChallengeComplete{ChallengeID: challengeID, RegistrationID: registration, Purpose: purpose, CompletionID: domain.CompletionID(strings.ToLower(fields["completionID"])), ProofID: fields["proofID"]})
			fields["proofID"] = ""
		}
		if err != nil {
			var rate application.ChallengeRateLimited
			switch {
			case errors.As(err, &rate):
				seconds := max(int64(1), int64((rate.RetryAfter+time.Second-1)/time.Second))
				w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
				respond(429, "ChallengeRateLimited")
			case errors.Is(err, application.ErrChallengeCapacity):
				w.Header().Set("Retry-After", "60")
				respond(429, "ChallengeRateLimited")
			case errors.Is(err, application.ErrInvalidChallengeRequest):
				respond(400, "InvalidRequest")
			case errors.Is(err, application.ErrInvalidCode):
				respond(400, "InvalidCode")
			case errors.Is(err, application.ErrChallengeUnavailable):
				respond(404, "ChallengeUnavailable")
			case errors.Is(err, application.ErrChallengeIdentityConflict):
				respond(409, "ChallengeIdentityConflict")
			case errors.Is(err, application.ErrVerificationIdentityConflict):
				respond(409, "VerificationIdentityConflict")
			case errors.Is(err, application.ErrCompletionIdentityConflict):
				respond(409, "CompletionIdentityConflict")
			case errors.Is(err, application.ErrStorageOutcomeUnknown):
				respond(503, "FeedbackOutcomeUnknown")
			default:
				logger.Warn("challenge operation unavailable", "requestID", requestID)
				respond(503, "FeedbackStorageUnavailable")
			}
			return
		}
		writeJSON(w, status, result, logger)
	}), nil
}

func decodeChallengeFields(body []byte, keys []string) (map[string]string, error) {
	if !utf8.Valid(body) || !validEscapes(body) {
		return nil, errors.New("invalid challenge text")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	if err := uniqueKeys(d); err != nil {
		return nil, err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing challenge input")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil || len(raw) != len(keys) {
		return nil, errors.New("invalid challenge fields")
	}
	fields := make(map[string]string, len(keys))
	for _, key := range keys {
		value, exists := raw[key]
		if !exists || bytes.Equal(value, []byte("null")) {
			return nil, errors.New("missing challenge field")
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return nil, errors.New("invalid challenge field")
		}
		fields[key] = text
	}
	return fields, nil
}

func validPrivateID(value string) bool { return domain.ValidID(domain.SubmissionID(value)) }
func canonicalCode(value string) bool {
	if len(value) != 6 {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
func validProof(value string) bool {
	if len(value) < 32 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}
