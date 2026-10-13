package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestA8ConfigRequiresSeparateDiscoveryReference(t *testing.T) {
	cfg := Config{Version: 1, Protocol: Protocol, Project: project, Owner: "feedback", FixtureID: fixtureID, InitialInstant: "2026-10-13T09:00:00+03:00", ClockGeneration: "1", ControlCredentialFile: FileRef{Target: "feedback", Path: "/run/umir-fixtures/secrets/fixture-feedback"}, CredentialFiles: map[string]FileRef{"gateway-feedback": {Target: "feedback", Path: "/run/umir-fixtures/secrets/gateway-feedback"}, "feedback-auth": {Target: "feedback", Path: "/run/umir-fixtures/secrets/feedback-auth"}}, ControlSocket: "/run/umir-fixtures/control.sock", PrivatePort: 8080, PublicPort: 0, Origin: "http://127.0.0.1:12345", ServiceURLs: map[string]string{"auth": "http://auth:8080", "warehouse": "http://warehouse:8080", "serving": "http://serving:8080", "feedback": "http://feedback:8080", "gateway": "http://gateway:8082"}, StateFile: "/var/lib/umir-feedback/feedback.jsonl", Bootstrap: json.RawMessage("null"), Delivery: json.RawMessage(`{"smtpHost":"smtp","smtpPort":2525,"smtpTLS":"starttls","smtpCAFile":{"target":"feedback","path":"/run/umir-fixtures/secrets/synthetic-ca.pem"},"notificationProxy":"http://notification:3128","notificationCAFile":{"target":"feedback","path":"/run/umir-fixtures/secrets/synthetic-ca.pem"}}`)}
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(dir, "config.json")
	write := func() {
		t.Helper()
		b, e := json.Marshal(cfg)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(p, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	write()
	if _, _, e := LoadConfig(p); e != nil {
		t.Fatal(e)
	}
	delete(cfg.CredentialFiles, "feedback-auth")
	write()
	if _, _, e := LoadConfig(p); e == nil {
		t.Fatal("missing real discovery binding accepted")
	}
	cfg.CredentialFiles["feedback-auth"] = FileRef{Target: "feedback", Path: "/run/umir-fixtures/secrets/gateway-feedback"}
	write()
	if _, _, e := LoadConfig(p); e == nil {
		t.Fatal("reused Gateway credential accepted")
	}
	cfg.CredentialFiles["feedback-auth"] = FileRef{Target: "feedback", Path: "/run/umir-fixtures/secrets/feedback-auth"}
	cfg.ServiceURLs["auth"] = "http://untrusted:8080"
	write()
	if _, _, e := LoadConfig(p); e == nil {
		t.Fatal("unreviewed key source accepted")
	}
}

func TestA8AcceptedAuthTrailingSlashCanonicalizesForRealDiscovery(t *testing.T) {
	cfg := Config{Version: 1, Protocol: Protocol, Project: project, Owner: "feedback", FixtureID: fixtureID, InitialInstant: "2026-10-13T09:00:00+03:00", ClockGeneration: "1", ControlCredentialFile: FileRef{Target: "feedback", Path: "/run/umir-fixtures/secrets/fixture-feedback"}, CredentialFiles: map[string]FileRef{"gateway-feedback": {Target: "feedback", Path: "/run/umir-fixtures/secrets/gateway-feedback"}, "feedback-auth": {Target: "feedback", Path: "/run/umir-fixtures/secrets/feedback-auth"}}, ControlSocket: "/run/umir-fixtures/control.sock", PrivatePort: 8080, Origin: "http://127.0.0.1:12345", ServiceURLs: map[string]string{"auth": "http://auth:8080/", "warehouse": "http://warehouse:8080/", "serving": "http://serving:8080/", "feedback": "http://feedback:8080/", "gateway": "http://gateway:8082/"}, StateFile: "/var/lib/umir-feedback/feedback.jsonl", Bootstrap: json.RawMessage("null"), Delivery: json.RawMessage(`{"smtpHost":"smtp","smtpPort":2525,"smtpTLS":"starttls","smtpCAFile":{"target":"feedback","path":"/run/umir-fixtures/secrets/synthetic-ca.pem"},"notificationProxy":"http://notification:3128","notificationCAFile":{"target":"feedback","path":"/run/umir-fixtures/secrets/synthetic-ca.pem"}}`)}
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(dir, "config.json")
	b, e := json.Marshal(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
	loaded, _, e := LoadConfig(p)
	if e != nil {
		t.Fatal(e)
	}
	if loaded.ServiceURLs["auth"]+"/internal/auth/v1/jwks" != "http://auth:8080/internal/auth/v1/jwks" {
		t.Fatal("accepted trailing slash produces a double-path Auth discovery URL")
	}
}
