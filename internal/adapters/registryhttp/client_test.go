package registryhttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChangesUsesAuthRegistryContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/internal/auth/v1/registry" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("since") != "12" || r.URL.Query().Get("after") != "cursor-1" || r.URL.Query().Get("limit") != "1000" {
			t.Fatalf("unexpected query: %s", r.URL.RawQuery)
		}
		if r.Header.Get("X-Umir-Service-Key") != "synthetic-key" {
			t.Fatal("service key header is missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"registryRevision":15,"complete":true,"entries":[],"tombstones":[{"restaurantID":"123e4567-e89b-42d3-a456-426614174000","orgID":"123e4567-e89b-42d3-a456-426614174001","lifecycleVersion":14}]}`))
	}))
	defer server.Close()

	client, err := New(server.URL, "synthetic-key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.Changes(context.Background(), 12, "cursor-1")
	if err != nil {
		t.Fatal(err)
	}
	if page.Revision != 15 || !page.Complete || page.NextAfter != "" || len(page.Tombstones) != 1 || page.Tombstones[0].LifecycleVersion != 14 {
		t.Fatalf("unexpected page: %#v", page)
	}
}

func TestChangesRejectsInvalidPageShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"registryRevision":15,"complete":false,"entries":[],"tombstones":[]}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "synthetic-key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Changes(context.Background(), 12, ""); err == nil {
		t.Fatal("missing nextAfter must be rejected")
	}
}
