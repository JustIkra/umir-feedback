package vpn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDownloadSubscriptionRejectsRedirectAndUnboundedBody(t *testing.T) {
	redirectTargetHits := 0
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectTargetHits++
	}))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	if _, err := downloadSubscription(context.Background(), redirect.URL, redirect.Client()); err == nil {
		t.Fatal("download accepted a redirect")
	}
	if redirectTargetHits != 0 {
		t.Fatal("download followed a redirect to a different origin")
	}

	large := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if _, err := writer.Write([]byte(strings.Repeat("x", maxSubscriptionBytes+1))); err != nil {
			t.Errorf("write oversized fixture: %v", err)
		}
	}))
	defer large.Close()
	if _, err := downloadSubscription(context.Background(), large.URL, large.Client()); err == nil {
		t.Fatal("download accepted a body larger than the configured limit")
	}
}

func TestDownloadSubscriptionRequiresHTTPSWithoutURLCredentials(t *testing.T) {
	for _, rawURL := range []string{"http://example.invalid/subscription", "https://user:pass@example.invalid/subscription", "https://example.invalid/subscription#fragment"} {
		if _, err := downloadSubscription(context.Background(), rawURL, http.DefaultClient); err == nil {
			t.Fatalf("download accepted unsafe URL %q", rawURL)
		}
	}
}
