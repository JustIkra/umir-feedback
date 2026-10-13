package httpapi

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
)

func TestA8UnauthenticatedScopedIntakeIsRecognized(t *testing.T) {
	h := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, func() bool { return true })
	r := httptest.NewRequest("POST", "/internal/feedback/v2/submissions/app", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("private app intake must require service authentication, got %d", w.Code)
	}
}
