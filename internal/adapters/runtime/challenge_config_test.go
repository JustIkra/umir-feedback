package runtime

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestOTPDisabledKeepsFeedbackConfigurationIndependent(t *testing.T) {
	env := testEnv(t)
	env["FEEDBACK_OTP_PEPPER"] = "invalid-unused"
	c, err := LoadConfig(func(k string) string { return env[k] })
	if err != nil || c.OTP.Enabled {
		t.Fatalf("disabled feature: %v", err)
	}
}

func TestOTPEnabledRequiresKeysAndRealOperatorDetails(t *testing.T) {
	env := testEnv(t)
	env["FEEDBACK_OTP_ENABLED"] = "true"
	env["FEEDBACK_SERVICE_TOKEN"] = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	env["FEEDBACK_OTP_PEPPER"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	env["FEEDBACK_OTP_ENCRYPTION_KEY"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	env["FEEDBACK_OPERATOR_NAME"] = "Тестовая организация"
	env["FEEDBACK_OPERATOR_ADDRESS"] = "Тестовый адрес, дом 1"
	env["FEEDBACK_REGISTRATION_SENDER_NAME"] = "Ровный ход"
	c, err := LoadConfig(func(k string) string { return env[k] })
	if err != nil || !c.OTP.Enabled || c.OTP.MaxChallenges != 4096 {
		t.Fatalf("enabled feature: %v", err)
	}
	for _, key := range []string{"FEEDBACK_SERVICE_TOKEN", "FEEDBACK_OTP_PEPPER", "FEEDBACK_OTP_ENCRYPTION_KEY", "FEEDBACK_OPERATOR_NAME", "FEEDBACK_OPERATOR_ADDRESS", "FEEDBACK_REGISTRATION_SENDER_NAME"} {
		t.Run(key, func(t *testing.T) {
			original := env[key]
			env[key] = ""
			_, err := LoadConfig(func(k string) string { return env[k] })
			env[key] = original
			if err == nil || strings.Contains(err.Error(), original) {
				t.Fatalf("missing config error: %v", err)
			}
		})
	}
	env["FEEDBACK_OPERATOR_NAME"] = "[Наименование оператора]"
	if _, err := LoadConfig(func(k string) string { return env[k] }); err == nil {
		t.Fatal("placeholder accepted")
	}
	env["FEEDBACK_OPERATOR_NAME"] = "Тестовая организация"
	env["FEEDBACK_OTP_MAX_CHALLENGES"] = "4097"
	if _, err := LoadConfig(func(k string) string { return env[k] }); err == nil {
		t.Fatal("unbounded capacity accepted")
	}
}
