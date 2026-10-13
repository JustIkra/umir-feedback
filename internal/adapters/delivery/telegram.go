package delivery

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

const telegramResponseLimit = 64 * 1024

var (
	botTokenPattern = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)
	chatIDPattern   = regexp.MustCompile(`^(?:-?[0-9]+|@[A-Za-z0-9_]{5,32})$`)
)

type TelegramConfig struct {
	Token         string
	ChatID        string
	ProxyURL      string
	ProxyUsername string
	ProxyPassword string
}

type RequestObserver interface {
	Request(context.Context, domain.Delivery, *http.Request) error
}
type TelegramOptions struct {
	RootCAs  *x509.CertPool
	Observer RequestObserver
}

type Telegram struct {
	observer RequestObserver
	client   *http.Client
	endpoint string
	chatID   string
}

func NewTelegram(config TelegramConfig) (*Telegram, error) {
	return newTelegram(config, nil)
}

func NewTelegramWithOptions(config TelegramConfig, opts TelegramOptions) (*Telegram, error) {
	t, e := newTelegram(config, opts.RootCAs)
	if e != nil {
		return nil, e
	}
	t.observer = opts.Observer
	return t, nil
}

func newTelegram(config TelegramConfig, roots *x509.CertPool) (*Telegram, error) {
	if !botTokenPattern.MatchString(config.Token) {
		return nil, errors.New("invalid FEEDBACK_TELEGRAM_BOT_TOKEN")
	}
	if !chatIDPattern.MatchString(config.ChatID) {
		return nil, errors.New("invalid FEEDBACK_TELEGRAM_CHAT_ID")
	}
	proxyURL, err := url.Parse(config.ProxyURL)
	if err != nil || !validProxyURL(proxyURL) {
		return nil, errors.New("invalid FEEDBACK_TELEGRAM_PROXY_URL")
	}
	if strings.TrimSpace(config.ProxyUsername) == "" || strings.Contains(config.ProxyUsername, ":") || strings.TrimSpace(config.ProxyPassword) == "" || len(config.ProxyUsername) > 256 || len(config.ProxyPassword) > 256 || strings.ContainsAny(config.ProxyUsername+config.ProxyPassword, "\r\n\x00") {
		return nil, errors.New("invalid FEEDBACK_TELEGRAM_PROXY_USERNAME or FEEDBACK_TELEGRAM_PROXY_PASSWORD")
	}
	proxyAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(config.ProxyUsername+":"+config.ProxyPassword))
	transport := &http.Transport{
		Proxy:                  http.ProxyURL(proxyURL),
		ProxyConnectHeader:     http.Header{"Proxy-Authorization": []string{proxyAuth}},
		DialContext:            (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "api.telegram.org", RootCAs: roots},
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  10 * time.Second,
		MaxResponseHeaderBytes: 16 * 1024,
		MaxIdleConns:           2,
		MaxIdleConnsPerHost:    2,
		IdleConnTimeout:        30 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   sessionTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Telegram{
		client: client, endpoint: "https://api.telegram.org/bot" + config.Token + "/sendMessage", chatID: config.ChatID,
	}, nil
}

func (t *Telegram) Close() error {
	if t == nil || t.client == nil {
		return nil
	}
	t.client.CloseIdleConnections()
	return nil
}

func validProxyURL(proxyURL *url.URL) bool {
	if proxyURL == nil || proxyURL.Scheme != "http" || proxyURL.Host == "" || proxyURL.User != nil || proxyURL.RawQuery != "" || proxyURL.Fragment != "" || (proxyURL.Path != "" && proxyURL.Path != "/") {
		return false
	}
	if strings.ContainsAny(proxyURL.Hostname(), " \t\r\n") {
		return false
	}
	port, err := strconv.Atoi(proxyURL.Port())
	return err == nil && port > 0 && port <= 65535
}

func (t *Telegram) Send(ctx context.Context, delivery domain.Delivery) domain.DeliveryResult {
	if delivery.Job.Channel != domain.Telegram {
		return permanentTelegram("telegram_invalid_channel")
	}
	if !messageIDPattern.MatchString(string(delivery.Submission.ID)) || delivery.Submission.ReceivedAt.IsZero() {
		return permanentTelegram("telegram_invalid_submission")
	}
	payload := struct {
		ChatID string `json:"chat_id"`
		Text   string `json:"text"`
	}{ChatID: t.chatID, Text: telegramText(delivery.Submission)}
	body, err := json.Marshal(payload)
	if err != nil {
		return permanentTelegram("telegram_invalid_submission")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, strings.NewReader(string(body)))
	if err != nil {
		return permanentTelegram("telegram_invalid_request")
	}
	request.Header.Set("Content-Type", "application/json")
	if t.observer != nil {
		if e := t.observer.Request(ctx, delivery, request); e != nil {
			return permanentTelegram("telegram_observation_error")
		}
	}
	response, err := t.client.Do(request)
	if err != nil {
		return transientTelegram("telegram_transport_error", 0)
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		if err := response.Body.Close(); err != nil {
			return transientTelegram("telegram_transport_error", 0)
		}
		return permanentTelegram("telegram_redirect_rejected")
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, telegramResponseLimit+1))
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil || len(responseBody) > telegramResponseLimit {
		return transientTelegram("telegram_invalid_response", 0)
	}
	var result telegramResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		if response.StatusCode == http.StatusTooManyRequests {
			return transientTelegram("telegram_rate_limited", 0)
		}
		if response.StatusCode >= 500 {
			return transientTelegram("telegram_provider_error", 0)
		}
		return transientTelegram("telegram_invalid_response", 0)
	}
	if response.StatusCode == http.StatusTooManyRequests || result.ErrorCode == http.StatusTooManyRequests {
		return transientTelegram("telegram_rate_limited", retryAfter(result.Parameters.RetryAfter))
	}
	if response.StatusCode >= 500 || result.ErrorCode >= 500 {
		return transientTelegram("telegram_provider_error", 0)
	}
	if response.StatusCode >= 400 || !result.OK {
		return permanentTelegram("telegram_rejected")
	}
	if result.Result.MessageID <= 0 {
		return transientTelegram("telegram_invalid_response", 0)
	}
	return domain.DeliveryResult{Success: true, Code: "telegram_sent", ProviderID: strconv.FormatInt(result.Result.MessageID, 10)}
}

func telegramText(submission domain.Submission) string {
	return fmt.Sprintf("Новое обращение\nID: %s\nВремя: %s\nПроверьте служебную почту.", submission.ID, submission.ReceivedAt.In(moscowLocation()).Format(time.RFC3339))
}

type telegramResponse struct {
	OK        bool `json:"ok"`
	ErrorCode int  `json:"error_code"`
	Result    struct {
		MessageID int64 `json:"message_id"`
	} `json:"result"`
	Parameters struct {
		RetryAfter int64 `json:"retry_after"`
	} `json:"parameters"`
}

func retryAfter(seconds int64) time.Duration {
	if seconds <= 0 {
		return 0
	}
	if seconds > int64((24*time.Hour)/time.Second) {
		return 24 * time.Hour
	}
	if seconds > math.MaxInt64/int64(time.Second) {
		return 24 * time.Hour
	}
	return time.Duration(seconds) * time.Second
}

func transientTelegram(code string, retryAfter time.Duration) domain.DeliveryResult {
	return domain.DeliveryResult{Code: code, RetryAfter: retryAfter}
}

func permanentTelegram(code string) domain.DeliveryResult {
	return domain.DeliveryResult{Permanent: true, Code: code}
}
