package bootstrap

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestA8ReviewExportRejectsTrailingForeignScope(t *testing.T) {
	restaurant := "44444444-4444-4444-8444-444444444444"
	foreign := "55555555-5555-4555-8555-555555555555"
	body := `{"tenantID":"` + restaurant + `","items":[]}` + "\n" + `{"tenantID":"` + foreign + `","items":[{"message":"foreign synthetic content"}]}`
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, body); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	original := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = original }()
	file := filepath.Join(t.TempDir(), "session")
	if err := os.WriteFile(file, []byte("synthetic-private-session"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := Command(context.Background(), []string{"export", "--gateway-url", server.URL, "--restaurant-id", restaurant, "--session-file", file}, func(string) string { return "" }, &output)
	if err == nil || output.Len() != 0 {
		t.Fatalf("malformed scoped response accepted: err=%v foreign bytes emitted=%v body=%s", err, strings.Contains(output.String(), foreign), output.String())
	}
}
