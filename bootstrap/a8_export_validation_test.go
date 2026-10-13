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

func TestA8ExportFrozenShapeAndFullConsumption(t *testing.T) {
	tenant := "44444444-4444-4444-8444-444444444444"
	foreign := "55555555-5555-4555-8555-555555555555"
	row := `{"receipt":{"submissionID":"66666666-6666-4666-8666-666666666666","receivedAt":"2026-10-13T09:00:00+03:00"},"scope":{"kind":"app","userID":"22222222-2222-4222-8222-222222222222","orgID":"33333333-3333-4333-8333-333333333333","tenantID":"` + tenant + `"},"state":"accepted"}`
	empty := `{"tenantID":"` + tenant + `","items":[]}`
	page := `{"tenantID":"` + tenant + `","items":[` + row + `]}`
	cases := []struct {
		name, body string
		valid      bool
	}{
		{"empty", empty, true},
		{"populated", page + " \n\t", true},
		{"duplicate-state", strings.Replace(page, `"accepted"`, `"duplicate"`, 1), true},
		{"cursor", strings.TrimSuffix(page, "}") + `,"nextCursor":"opaque"}`, true},
		{"unicode-cursor", strings.TrimSuffix(empty, "}") + `,"nextCursor":"\ud83d\ude00"}`, true},
		{"unpaired-surrogate", strings.TrimSuffix(empty, "}") + `,"nextCursor":"\ud800"}`, false},
		{"invalid-utf8", strings.TrimSuffix(empty, "}") + ",\"nextCursor\":\"" + string([]byte{0xff}) + "\"}", false},
		{"wrong-case-top-key", strings.Replace(empty, "tenantID", "TenantID", 1), false},
		{"wrong-case-nested-key", strings.Replace(page, "receipt", "Receipt", 1), false},
		{"trailing-object", empty + `{"tenantID":"` + foreign + `","items":[]}`, false},
		{"trailing-scalar", empty + " true", false},
		{"trailing-garbage", empty + " !", false},
		{"duplicate-key", strings.Replace(empty, `"items":[]`, `"items":[],"items":[]`, 1), false},
		{"duplicate-nested-key", strings.Replace(page, `"state":"accepted"`, `"state":"accepted","state":"accepted"`, 1), false},
		{"unknown-row-field", strings.Replace(page, `"state":"accepted"`, `"state":"accepted","message":"synthetic-private-content"`, 1), false},
		{"foreign-page", strings.Replace(empty, tenant, foreign, 1), false},
		{"foreign-row", strings.Replace(page, `"tenantID":"`+tenant+`"},"state"`, `"tenantID":"`+foreign+`"},"state"`, 1), false},
		{"missing-items", `{"tenantID":"` + tenant + `"}`, false},
		{"null-items", `{"tenantID":"` + tenant + `","items":null}`, false},
		{"null-row", `{"tenantID":"` + tenant + `","items":[null]}`, false},
		{"missing-receipt", strings.Replace(page, `"receipt":`+strings.Split(strings.TrimPrefix(row, `{"receipt":`), `,"scope"`)[0]+",", "", 1), false},
		{"invalid-receipt-id", strings.Replace(page, "66666666-6666-4666-8666-666666666666", "invalid", 1), false},
		{"invalid-instant", strings.Replace(page, "2026-10-13T09:00:00+03:00", "2026-10-13T09:00:00", 1), false},
		{"invalid-state", strings.Replace(page, `"accepted"`, `"sent"`, 1), false},
		{"missing-state", strings.Replace(page, `,"state":"accepted"`, "", 1), false},
		{"null-cursor", strings.TrimSuffix(empty, "}") + `,"nextCursor":null}`, false},
		{"empty-cursor", strings.TrimSuffix(empty, "}") + `,"nextCursor":""}`, false},
	}
	var body string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body = tc.body
			var out bytes.Buffer
			err := Command(context.Background(), []string{"export", "--gateway-url", server.URL, "--restaurant-id", tenant, "--session-file", file}, func(string) string { return "" }, &out)
			if tc.valid {
				if err != nil || out.String() != body {
					t.Fatalf("valid frozen page refused: %v", err)
				}
			} else if err == nil || out.Len() != 0 {
				t.Fatalf("invalid page emitted bytes: err=%v bytes=%d", err, out.Len())
			}
		})
	}
}
