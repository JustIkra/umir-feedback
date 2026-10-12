package runtime

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"

	"github.com/JustIkra/umir-feedback/internal/adapters/delivery"
)

type OTPConfig struct {
	Enabled       bool
	MaxChallenges int
	ServiceToken  string
	Secrets       *ChallengeCrypto
	Mail          delivery.RegistrationMailConfig
}

func loadOTPConfig(getenv func(string) string, smtp *delivery.SMTP) (OTPConfig, error) {
	var c OTPConfig
	switch getenv("FEEDBACK_OTP_ENABLED") {
	case "", "false":
		return c, nil
	case "true":
		c.Enabled = true
	default:
		return c, errors.New("invalid FEEDBACK_OTP_ENABLED")
	}
	c.MaxChallenges = 4096
	if value := getenv("FEEDBACK_OTP_MAX_CHALLENGES"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 4096 {
			return c, errors.New("invalid FEEDBACK_OTP_MAX_CHALLENGES")
		}
		c.MaxChallenges = n
	}
	token, err := secretValue(getenv, "FEEDBACK_SERVICE_TOKEN")
	if err != nil {
		return c, err
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(decoded) < 32 || len(decoded) > 128 {
		clear(decoded)
		return c, errors.New("invalid FEEDBACK_SERVICE_TOKEN")
	}
	clear(decoded)
	c.ServiceToken = token
	pepper, err := otpKey(getenv, "FEEDBACK_OTP_PEPPER")
	if err != nil {
		return c, err
	}
	defer clear(pepper)
	key, err := otpKey(getenv, "FEEDBACK_OTP_ENCRYPTION_KEY")
	if err != nil {
		return c, err
	}
	defer clear(key)
	c.Secrets, err = NewChallengeCrypto(pepper, key)
	if err != nil {
		return c, errors.New("invalid separate OTP keys")
	}
	c.Mail = delivery.RegistrationMailConfig{
		OperatorName:    getenv("FEEDBACK_OPERATOR_NAME"),
		OperatorAddress: getenv("FEEDBACK_OPERATOR_ADDRESS"),
		SenderName:      getenv("FEEDBACK_REGISTRATION_SENDER_NAME"),
		PrivacyURL:      getenv("FEEDBACK_PRIVACY_URL"),
	}
	if _, err := delivery.NewRegistrationSMTP(smtp, c.Mail); err != nil {
		return c, errors.New("invalid registration mail operator details")
	}
	return c, nil
}

func otpKey(getenv func(string) string, name string) ([]byte, error) {
	value, err := secretValue(getenv, name)
	if err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) != 32 {
		clear(decoded)
		return nil, fmt.Errorf("invalid %s", name)
	}
	return decoded, nil
}
