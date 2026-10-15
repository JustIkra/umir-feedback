package runtime

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/JustIkra/umir-feedback/internal/adapters/authkeys"
	"github.com/JustIkra/umir-feedback/internal/adapters/delivery"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

type Config struct {
	AuthURL, GatewayKey, DiscoveryKey        string
	AuthKeysMaxStale                         time.Duration
	ListenAddress, StorePath, ConsentVersion string
	MaxStoreBytes                            int64
	RetentionDays                            int
	Policy                                   domain.RetryPolicy
	PollInterval                             time.Duration
	SMTP                                     delivery.SMTPConfig
	Telegram                                 delivery.TelegramConfig
}

func LoadStoreConfig(getenv func(string) string) (Config, error) {
	var c Config
	c.ListenAddress = getenv("FEEDBACK_LISTEN_ADDRESS")
	if c.ListenAddress == "" {
		c.ListenAddress = "127.0.0.1:8084"
	}
	_, port, err := net.SplitHostPort(c.ListenAddress)
	if err != nil {
		return c, errors.New("invalid FEEDBACK_LISTEN_ADDRESS")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return c, errors.New("invalid FEEDBACK_LISTEN_ADDRESS")
	}
	c.StorePath = getenv("FEEDBACK_STORE_PATH")
	if !filepath.IsAbs(c.StorePath) || filepath.Clean(c.StorePath) != c.StorePath {
		return c, errors.New("invalid FEEDBACK_STORE_PATH")
	}
	c.MaxStoreBytes, err = strconv.ParseInt(getenv("FEEDBACK_MAX_STORE_BYTES"), 10, 64)
	if err != nil || c.MaxStoreBytes < 1024*1024 || c.MaxStoreBytes > 1<<40 {
		return c, errors.New("invalid FEEDBACK_MAX_STORE_BYTES")
	}
	c.RetentionDays, err = strconv.Atoi(getenv("FEEDBACK_RETENTION_DAYS"))
	if err != nil || c.RetentionDays < 1 || c.RetentionDays > 3650 {
		return c, errors.New("invalid FEEDBACK_RETENTION_DAYS")
	}
	c.ConsentVersion = getenv("FEEDBACK_CONSENT_VERSION")
	if c.ConsentVersion == "" || len(c.ConsentVersion) > 128 || strings.TrimSpace(c.ConsentVersion) != c.ConsentVersion {
		return c, errors.New("invalid FEEDBACK_CONSENT_VERSION")
	}
	for _, r := range c.ConsentVersion {
		if unicode.IsControl(r) {
			return c, errors.New("invalid FEEDBACK_CONSENT_VERSION")
		}
	}
	c.Policy = domain.DefaultRetryPolicy()
	if value := getenv("FEEDBACK_RETRY_MAX_ATTEMPTS"); value != "" {
		c.Policy.MaxAttempts, err = strconv.Atoi(value)
		if err != nil || c.Policy.MaxAttempts < 1 || c.Policy.MaxAttempts > 10 {
			return c, errors.New("invalid FEEDBACK_RETRY_MAX_ATTEMPTS")
		}
	}
	for _, parameter := range []struct {
		key      string
		target   *time.Duration
		min, max time.Duration
	}{
		{"FEEDBACK_RETRY_MAX_AGE", &c.Policy.MaxAge, time.Second, 24 * time.Hour},
		{"FEEDBACK_RETRY_MIN_DELAY", &c.Policy.MinDelay, time.Second, 15 * time.Minute},
		{"FEEDBACK_RETRY_MAX_DELAY", &c.Policy.MaxDelay, time.Second, 15 * time.Minute},
	} {
		if value := getenv(parameter.key); value != "" {
			d, err := time.ParseDuration(value)
			if err != nil || d < parameter.min || d > parameter.max {
				return c, fmt.Errorf("invalid %s", parameter.key)
			}
			*parameter.target = d
		}
	}
	if !c.Policy.Valid() {
		return c, errors.New("invalid FEEDBACK_RETRY_MIN_DELAY or FEEDBACK_RETRY_MAX_DELAY")
	}
	c.PollInterval = time.Second
	if value := getenv("FEEDBACK_WORKER_POLL_INTERVAL"); value != "" {
		c.PollInterval, err = time.ParseDuration(value)
		if err != nil || c.PollInterval < 100*time.Millisecond || c.PollInterval > time.Minute {
			return c, errors.New("invalid FEEDBACK_WORKER_POLL_INTERVAL")
		}
	}
	return c, nil
}

func LoadConfig(getenv func(string) string) (Config, error) {
	if value := getenv("FEEDBACK_OTP_ENABLED"); value != "" && value != "false" {
		return Config{}, errors.New("feedback OTP owner has moved to Auth")
	}
	c, err := LoadStoreConfig(getenv)
	if err != nil {
		return c, err
	}
	c.SMTP.Host = getenv("FEEDBACK_SMTP_HOST")
	c.SMTP.Port, err = strconv.Atoi(getenv("FEEDBACK_SMTP_PORT"))
	if err != nil {
		return c, errors.New("invalid FEEDBACK_SMTP_PORT")
	}
	c.SMTP.TLSMode = getenv("FEEDBACK_SMTP_TLS_MODE")
	parameters := []struct {
		key    string
		target *string
	}{
		{"FEEDBACK_SMTP_USERNAME", &c.SMTP.Username}, {"FEEDBACK_SMTP_PASSWORD", &c.SMTP.Password},
		{"FEEDBACK_MAIL_FROM", &c.SMTP.From}, {"FEEDBACK_MAIL_TO", &c.SMTP.To},
		{"FEEDBACK_TELEGRAM_BOT_TOKEN", &c.Telegram.Token}, {"FEEDBACK_TELEGRAM_CHAT_ID", &c.Telegram.ChatID},
		{"FEEDBACK_TELEGRAM_PROXY_USERNAME", &c.Telegram.ProxyUsername}, {"FEEDBACK_TELEGRAM_PROXY_PASSWORD", &c.Telegram.ProxyPassword},
	}
	for _, parameter := range parameters {
		value, err := secretValue(getenv, parameter.key)
		if err != nil {
			return c, err
		}
		*parameter.target = value
	}
	c.Telegram.ProxyURL = getenv("FEEDBACK_TELEGRAM_PROXY_URL")
	_, err = delivery.NewSMTP(c.SMTP)
	if err != nil {
		return c, err
	}
	telegram, err := delivery.NewTelegram(c.Telegram)
	if err != nil {
		return c, err
	}
	if err := telegram.Close(); err != nil {
		return c, errors.New("telegram client cleanup failed")
	}
	c.AuthKeysMaxStale = authkeys.DefaultMaxStale
	if value := getenv("FEEDBACK_AUTH_KEYS_MAX_STALE"); value != "" {
		c.AuthKeysMaxStale, err = time.ParseDuration(value)
		if err != nil || c.AuthKeysMaxStale < authkeys.MinMaxStale || c.AuthKeysMaxStale > authkeys.MaxMaxStale {
			return c, errors.New("invalid FEEDBACK_AUTH_KEYS_MAX_STALE")
		}
	}
	c.AuthURL = getenv("FEEDBACK_AUTH_URL")
	if c.AuthURL != "" {
		u, e := url.Parse(c.AuthURL)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
			return c, errors.New("invalid FEEDBACK_AUTH_URL")
		}
		c.GatewayKey, e = secretValue(getenv, "FEEDBACK_GATEWAY_KEY")
		if e != nil {
			return c, e
		}
		c.DiscoveryKey, e = secretValue(getenv, "FEEDBACK_AUTH_DISCOVERY_KEY")
		if e != nil {
			return c, e
		}
		if c.GatewayKey == c.DiscoveryKey {
			return c, errors.New("service credentials must differ")
		}
	}
	return c, nil
}

func secretValue(getenv func(string) string, key string) (string, error) {
	value, file := getenv(key), getenv(key+"_FILE")
	if value != "" && file != "" {
		return "", fmt.Errorf("configure only %s or %s_FILE", key, key)
	}
	if file == "" {
		if value == "" {
			return "", fmt.Errorf("missing %s", key)
		}
		return value, nil
	}
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return "", fmt.Errorf("invalid %s_FILE", key)
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("cannot read %s_FILE", key)
	}
	value = strings.TrimSuffix(strings.TrimSuffix(string(content), "\n"), "\r")
	if value == "" {
		return "", fmt.Errorf("empty %s_FILE", key)
	}
	return value, nil
}
