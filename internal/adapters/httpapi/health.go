package httpapi

import (
	"io"
	"log/slog"
	"net/http"
)

func NewHandler(logger *slog.Logger, intake Submitter, ready func() bool) http.Handler {
	mux := http.NewServeMux()
	health := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			return
		}
		if _, err := io.WriteString(w, "{\"status\":\"ok\"}\n"); err != nil {
			logger.Warn("health response failed")
		}
	}
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /healthz", health)
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if ready == nil || !ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			if r.Method != http.MethodHead {
				if _, err := io.WriteString(w, "{\"status\":\"unavailable\"}\n"); err != nil {
					logger.Warn("readiness response failed")
				}
			}
			return
		}
		health(w, r)
	})
	mux.HandleFunc("/feedback", feedbackHandler(logger, intake))
	return mux
}
