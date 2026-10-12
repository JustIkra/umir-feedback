package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{"FEEDBACK_STORE_PATH": filepath.Join(t.TempDir(), "journal.jsonl"), "FEEDBACK_MAX_STORE_BYTES": "67108864", "FEEDBACK_RETENTION_DAYS": "30", "FEEDBACK_CONSENT_VERSION": "v1", "FEEDBACK_SMTP_HOST": "smtp.example.org", "FEEDBACK_SMTP_PORT": "587", "FEEDBACK_SMTP_TLS_MODE": "starttls", "FEEDBACK_SMTP_USERNAME": "user", "FEEDBACK_SMTP_PASSWORD": "private-password", "FEEDBACK_MAIL_FROM": "service@example.org", "FEEDBACK_MAIL_TO": "operator@example.org", "FEEDBACK_TELEGRAM_BOT_TOKEN": "123:dummy", "FEEDBACK_TELEGRAM_CHAT_ID": "-123", "FEEDBACK_TELEGRAM_PROXY_URL": "http://telegram-vpn:8080", "FEEDBACK_TELEGRAM_PROXY_USERNAME": "proxy-user", "FEEDBACK_TELEGRAM_PROXY_PASSWORD": "proxy-password"}
}
func TestConfigValidatesWithoutLeakingValues(t *testing.T) {
	env := testEnv(t)
	c, err := LoadConfig(func(k string) string { return env[k] })
	if err != nil || c.RetentionDays != 30 || c.ListenAddress != "127.0.0.1:8084" {
		t.Fatalf("config %v", err)
	}
	for _, key := range []string{"FEEDBACK_MAX_STORE_BYTES", "FEEDBACK_RETENTION_DAYS", "FEEDBACK_CONSENT_VERSION", "FEEDBACK_SMTP_PORT"} {
		t.Run(key, func(t *testing.T) {
			old := env[key]
			env[key] = " \x00unsafe-secret-value"
			_, err := LoadConfig(func(k string) string { return env[k] })
			env[key] = old
			if err == nil || !strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "unsafe-secret-value") {
				t.Fatalf("unsafe error %v", err)
			}
		})
	}
}
func TestSecretFileRequiresClosedRegularFile(t *testing.T) {
	env := testEnv(t)
	secret := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(secret, []byte("hidden-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env["FEEDBACK_SMTP_PASSWORD"] = ""
	env["FEEDBACK_SMTP_PASSWORD_FILE"] = secret
	c, err := LoadConfig(func(k string) string { return env[k] })
	if err != nil || c.SMTP.Password != "hidden-value" {
		t.Fatalf("file %v", err)
	}
	if err := os.Chmod(secret, 0644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadConfig(func(k string) string { return env[k] })
	if err == nil {
		t.Fatal("accepted public secret")
	}
}
func TestNFCNormalizer(t *testing.T) {
	if (Normalizer{}).NFC("e\u0301") != "é" {
		t.Fatal("NFC not applied")
	}
}
