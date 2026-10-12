package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

type acceptedIntake struct{ calls int }

func (s *acceptedIntake) Submit(_ context.Context, in domain.Input) (domain.Receipt, bool, error) {
	s.calls++
	return domain.Receipt{SubmissionID: in.ID, ReceivedAt: time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("MSK", 10800))}, true, nil
}
func TestFeedbackStrictBoundary(t *testing.T) {
	valid := `{"submissionID":"123e4567-e89b-42d3-a456-426614174000","name":"Имя","email":"a@example.org","message":"текст","consent":{"accepted":true,"documentVersion":"v1"},"website":""}`
	for _, tc := range []struct {
		name, method, content, body string
		status                      int
	}{
		{"accepted", "POST", "application/json", valid, 201},
		{"charset", "POST", "application/json; charset=UTF-8", valid, 201},
		{"method", "GET", "application/json", valid, 405},
		{"type", "POST", "text/plain", valid, 415},
		{"size", "POST", "application/json", strings.Repeat("x", 16385), 413},
		{"unknown", "POST", "application/json", strings.Replace(valid, `"website"`, `"other"`, 1), 400},
		{"duplicate", "POST", "application/json", strings.Replace(valid, `"name":"Имя"`, `"name":"Имя","name":"second"`, 1), 400},
		{"casefold", "POST", "application/json", strings.Replace(valid, `"name"`, `"Name"`, 1), 400},
		{"invalidUTF8", "POST", "application/json", strings.Replace(valid, "Имя", string([]byte{255}), 1), 400},
		{"loneSurrogate", "POST", "application/json", strings.Replace(valid, "Имя", `\ud800`, 1), 400},
		{"pairedSurrogate", "POST", "application/json", strings.Replace(valid, "Имя", `\ud83d\ude00`, 1), 201},
		{"trailing", "POST", "application/json", valid + `{}`, 400},
		{"null", "POST", "application/json", strings.Replace(valid, `"Имя"`, `null`, 1), 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			submit := &acceptedIntake{}
			handler := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), submit, func() bool { return true })
			r := httptest.NewRequest(tc.method, "/feedback", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.content)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d body %s", w.Code, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("cache headers")
			}
			if tc.status != 201 && submit.calls != 0 {
				t.Fatal("invalid input dispatched")
			}
		})
	}
}
func TestReadinessDoesNotCheckProviders(t *testing.T) {
	h := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), &acceptedIntake{}, func() bool { return false })
	for _, tc := range []struct {
		path   string
		status int
	}{{"/healthz", 200}, {"/readyz", 503}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if w.Code != tc.status {
			t.Fatalf("%s=%d", tc.path, w.Code)
		}
	}
}
