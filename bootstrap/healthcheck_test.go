package bootstrap

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JustIkra/umir-feedback/internal/adapters/httpapi"
)

func TestContainerHealthcheckUsesProcessHealth(t *testing.T) {
	server := httptest.NewServer(httpapi.NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, func() bool { return false }))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	if err := Healthcheck(func(key string) string {
		if key == "FEEDBACK_LISTEN_ADDRESS" {
			return address
		}
		return ""
	}); err != nil {
		t.Fatal(err)
	}
}
