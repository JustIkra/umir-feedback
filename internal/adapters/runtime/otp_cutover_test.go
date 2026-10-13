package runtime

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestOTPCutoverRejectsRemovedOwnerConfiguration(t *testing.T) {
	env := testEnv(t)
	env["FEEDBACK_OTP_ENABLED"] = "true"
	env["FEEDBACK_SERVICE_TOKEN"] = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	env["FEEDBACK_OTP_PEPPER"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	env["FEEDBACK_OTP_ENCRYPTION_KEY"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	env["FEEDBACK_OPERATOR_NAME"] = "Тестовая организация"
	env["FEEDBACK_OPERATOR_ADDRESS"] = "Тестовый адрес, дом 1"
	env["FEEDBACK_REGISTRATION_SENDER_NAME"] = "Ровный ход"
	if _, err := LoadConfig(func(key string) string { return env[key] }); err == nil {
		t.Fatal("Feedback accepted configuration for the removed OTP owner")
	}
}
