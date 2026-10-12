package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

type challengeSpy struct {
	calls int
	err   error
	code  string
}

func (s *challengeSpy) Begin(context.Context, application.ChallengeBegin) (domain.ChallengeReceipt, error) {
	s.calls++
	return domain.ChallengeReceipt{}, s.err
}
func (s *challengeSpy) Resend(context.Context, application.ChallengeResend) (domain.ChallengeReceipt, error) {
	s.calls++
	return domain.ChallengeReceipt{}, s.err
}
func (s *challengeSpy) Verify(_ context.Context, in application.ChallengeVerify) (domain.ChallengeProof, error) {
	s.calls++
	s.code = in.Code
	return domain.ChallengeProof{ProofID: "opaque"}, s.err
}
func (s *challengeSpy) Complete(context.Context, application.ChallengeComplete) (domain.ChallengeCompletion, error) {
	s.calls++
	return domain.ChallengeCompletion{}, s.err
}

const challengeTestID = "2f6b7c1e-4a3d-4e8b-9c2a-1d5e6f7a8b9c"

func TestChallengeHTTPAuthenticatesBeforeParsing(t *testing.T) {
	var logs bytes.Buffer
	core := &challengeSpy{}
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	h, err := NewChallengeHandler(slog.New(slog.NewTextHandler(&logs, nil)), core, token)
	if err != nil {
		t.Fatal(err)
	}
	for _, auth := range []string{"", "Bearer wrong", "bearer " + token, "Bearer " + token + " "} {
		r := httptest.NewRequest("POST", "/internal/v1/email-challenges", strings.NewReader("{invalid"))
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), "Unauthorized") {
			t.Fatalf("status %d body %s", w.Code, w.Body)
		}
	}
	if core.calls != 0 || strings.Contains(logs.String(), token) {
		t.Fatal("unauthenticated dispatch or secret logging")
	}
	if _, err := NewChallengeHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), core, "short"); err == nil {
		t.Fatal("weak service token accepted")
	}
}

func TestChallengeHTTPStrictBodiesAndOutcomeEnvelope(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	core := &challengeSpy{}
	h, err := NewChallengeHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), core, token)
	if err != nil {
		t.Fatal(err)
	}
	begin := `{"registrationID":"` + challengeTestID + `","email":"a@example.org","purpose":"operator-registration"}`
	for _, body := range []string{`null`, `[]`, begin + ` {}`, strings.Replace(begin, `"email"`, `"Email"`, 1), strings.Replace(begin, `"email":"a@example.org"`, `"email":null`, 1), strings.Replace(begin, `"purpose":"operator-registration"`, `"purpose":"operator-registration","purpose":"operator-registration"`, 1), strings.Replace(begin, "a@example.org", `a\ud800@example.org`, 1), strings.Replace(begin, "a@example.org", "A <a@example.org>", 1), strings.Replace(begin, challengeTestID, "not-a-uuid", 1), strings.Repeat(" ", 8193)} {
		r := httptest.NewRequest("POST", "/internal/v1/email-challenges", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("invalidbody status %d", w.Code)
		}
	}
	if core.calls != 0 {
		t.Fatal("invalid body dispatched")
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{nil, 202, ""},
		{application.ErrChallengeIdentityConflict, 409, "ChallengeIdentityConflict"},
		{application.ErrStorageOutcomeUnknown, 503, "FeedbackOutcomeUnknown"},
		{application.ErrStorageUnavailable, 503, "FeedbackStorageUnavailable"},
		{application.ChallengeRateLimited{RetryAfter: 1500 * time.Millisecond}, 429, "ChallengeRateLimited"},
	} {
		core.err = tc.err
		r := httptest.NewRequest("POST", "/internal/v1/email-challenges", strings.NewReader(begin))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("status=%d want=%d", w.Code, tc.status)
		}
		if tc.code != "" {
			var body errorBody
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Code != tc.code || body.Message != tc.code || body.RequestID == "" {
				t.Fatalf("bad envelope %s", w.Body)
			}
		}
		if tc.status == 429 && w.Header().Get("Retry-After") != "2" {
			t.Fatal("incorrect retry-after")
		}
	}
}

func TestChallengeHTTPVerificationDispatchKeepsCanonicalCodePrivate(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	core := &challengeSpy{err: application.ErrInvalidCode}
	h, err := NewChallengeHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), core, token)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"registrationID":"` + challengeTestID + `","purpose":"operator-registration","verificationID":"` + challengeTestID + `","code":"000007"}`
	r := httptest.NewRequest("POST", "/internal/v1/email-challenges/"+challengeTestID+"/verify", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 || core.code != "000007" || strings.Contains(w.Body.String(), "000007") {
		t.Fatal("verification boundary failed")
	}
	core.calls = 0
	r = httptest.NewRequest("POST", "/internal/v1/email-challenges/"+challengeTestID+"/verify?to=other@example.org", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 || core.calls != 0 {
		t.Fatal("query relay accepted")
	}
}
