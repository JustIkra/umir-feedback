package httpapi

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthContract(t *testing.T) {
	t.Parallel()
	handler := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	for _, tc := range []struct {
		method string
		path   string
		status int
		body   string
	}{
		{http.MethodGet, "/health", http.StatusOK, "{\"status\":\"ok\"}\n"},
		{http.MethodHead, "/health", http.StatusOK, ""},
		{http.MethodPost, "/health", http.StatusMethodNotAllowed, ""},
		{http.MethodGet, "/health/", http.StatusNotFound, ""},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			t.Parallel()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d", response.Code, tc.status)
			}
			if tc.status == http.StatusOK {
				if response.Body.String() != tc.body {
					t.Fatalf("body = %q, want %q", response.Body.String(), tc.body)
				}
				if response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("unexpected health headers: %v", response.Header())
				}
			}
		})
	}
}

type failedWriter struct {
	header http.Header
}

func (w *failedWriter) Header() http.Header { return w.header }
func (*failedWriter) WriteHeader(int)       {}
func (*failedWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func TestHealthHandlesWriteFailure(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	handler := NewHandler(slog.New(slog.NewTextHandler(&logs, nil)), nil, nil)
	handler.ServeHTTP(&failedWriter{header: make(http.Header)}, httptest.NewRequest(http.MethodGet, "/health", nil))
	if !bytes.Contains(logs.Bytes(), []byte("health response failed")) {
		t.Fatalf("write failure was not handled: %s", logs.String())
	}
}
