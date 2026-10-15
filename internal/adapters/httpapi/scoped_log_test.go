package httpapi

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

type failingScopedStore struct{}

func (failingScopedStore) SubmitScoped(context.Context, domain.Namespace, domain.Input) (domain.Receipt, bool, error) {
	return domain.Receipt{}, false, application.ErrStorageUnavailable
}
func (failingScopedStore) ListScoped(context.Context, string, string) ([]domain.Submission, error) {
	return nil, application.ErrStorageUnavailable
}

type fixedVerifier struct{ ns domain.Namespace }

func (v fixedVerifier) Validate(context.Context, string, string) (domain.Namespace, error) {
	return v.ns, nil
}

func TestScopedHandlerLogsStorageUnavailableWithoutPersonalData(t *testing.T) {
	ns := domain.Namespace{Kind: "app", UserID: "11111111-1111-4111-8111-111111111111", OrgID: "22222222-2222-4222-8222-222222222222", TenantID: "33333333-3333-4333-8333-333333333333"}
	var logs bytes.Buffer
	handler, err := NewScopedHandler(ScopedOptions{Logger: slog.New(slog.NewJSONHandler(&logs, nil)), Intake: failingScopedStore{}, Reader: failingScopedStore{}, Verifier: fixedVerifier{ns}, ServiceKey: "service-key-secret", Now: time.Now, Ready: func() bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"context":{"kind":"app","userID":"` + ns.UserID + `","orgID":"` + ns.OrgID + `","tenantID":"` + ns.TenantID + `"},"submission":{"submissionID":"123e4567-e89b-42d3-a456-426614174000","name":"Secret Person Name","email":"secret.person@example.org","phone":"+7 900 000-11-22","message":"secret message body","consent":{"accepted":true,"documentVersion":"v1"},"website":""}}`
	requests := []*http.Request{
		httptest.NewRequest("POST", "/internal/feedback/v2/submissions/app", strings.NewReader(body)),
		httptest.NewRequest("GET", "/internal/feedback/v2/r/"+ns.TenantID+"/submissions", nil),
	}
	requests[0].Header.Set("Content-Type", "application/json")
	for _, r := range requests {
		r.Header.Set("X-Umir-Service-Key", "service-key-secret")
		r.Header.Set("Authorization", "Bearer secret-token-value")
		r.RemoteAddr = "198.51.100.77:4000"
		r.Header.Set("X-Forwarded-For", "198.51.100.77")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 503 || !strings.Contains(w.Body.String(), "FeedbackStorageUnavailable") {
			t.Fatalf("%s: %d %s", r.URL.Path, w.Code, w.Body.String())
		}
	}
	raw := logs.String()
	if got := strings.Count(raw, `"msg":"feedback storage unavailable"`); got != 2 || !strings.Contains(raw, `"level":"WARN"`) {
		t.Fatalf("storage unavailable warnings %d: %s", got, raw)
	}
	for _, secret := range []string{"Secret Person", "secret.person", "example.org", "+7 900", "secret message", "secret-token", "service-key", "198.51.100.77", ns.UserID, ns.OrgID, ns.TenantID} {
		if strings.Contains(raw, secret) {
			t.Fatalf("log contains %q: %s", secret, raw)
		}
	}
}
