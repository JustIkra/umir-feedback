package delivery

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/smtp"
	"net/textproto"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

func TestNewSMTPRejectsUnsafeConfiguration(t *testing.T) {
	tests := []struct {
		name string
		edit func(*SMTPConfig)
	}{
		{name: "unknown TLS mode", edit: func(c *SMTPConfig) { c.TLSMode = "optional" }},
		{name: "invalid port", edit: func(c *SMTPConfig) { c.Port = 65536 }},
		{name: "header injection in from", edit: func(c *SMTPConfig) { c.From = "ops@example.test\r\nBcc: victim@example.test" }},
		{name: "header injection in to", edit: func(c *SMTPConfig) { c.To = "ops@example.test\nBcc: victim@example.test" }},
		{name: "partial credentials", edit: func(c *SMTPConfig) { c.Password = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := validSMTPConfig()
			tt.edit(&config)
			if _, err := NewSMTP(config); err == nil {
				t.Fatal("NewSMTP accepted invalid configuration")
			}
		})
	}
}

func TestNewSMTPErrorDoesNotExposeConfiguration(t *testing.T) {
	config := validSMTPConfig()
	config.Password = "smtp-private-password"
	config.From = "private-address@example.test\r\nBcc: victim@example.test"
	_, err := NewSMTP(config)
	if err == nil {
		t.Fatal("NewSMTP accepted header injection")
	}
	if strings.Contains(err.Error(), config.Password) || strings.Contains(err.Error(), config.From) {
		t.Fatalf("configuration value leaked in error: %q", err)
	}
}

func TestSMTPUsesVerifiedTLSAndStableMIMEMessage(t *testing.T) {
	server, roots := newSMTPTestServer(t, "tls", 250)
	config := validSMTPConfig()
	config.Port = server.port
	sender, err := newSMTPWithRoots(config, roots)
	if err != nil {
		t.Fatal(err)
	}
	result := sender.Send(context.Background(), testDelivery(domain.SMTP))
	if !result.Success || result.Permanent || result.Code != "smtp_sent" || result.ProviderID != "" {
		t.Fatalf("unexpected SMTP result: %+v", result)
	}
	message := <-server.messages
	for _, required := range []string{
		`From: "Feedback" <feedback@example.test>`,
		`To: "Operations" <ops@example.test>`,
		"Reply-To: <alice-private@example.test>",
		"Subject: New feedback: sub-123",
		"Message-ID: <sub-123@example.test>",
		"Content-Type: text/plain; charset=utf-8",
	} {
		if !strings.Contains(message, required) {
			t.Errorf("message lacks %q: %q", required, message)
		}
	}
	if strings.Contains(message, "Bcc:") {
		t.Fatalf("message contains unexpected Bcc header: %q", message)
	}
	parts := strings.SplitN(message, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatalf("message has no MIME body separator: %q", message)
	}
	body, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(parts[1], "\r\n", ""))
	if err != nil {
		t.Fatalf("decode MIME body: %v", err)
	}
	for _, required := range []string{"Alice Private", "alice-private@example.test", "+15551234567", "secret message body", "2026-10-12T13:30:00+03:00"} {
		if !strings.Contains(string(body), required) {
			t.Errorf("decoded message body lacks %q: %q", required, body)
		}
	}
}

func TestSMTPOmitsReplyToForNonASCIIAddressAndKeepsItInTheBody(t *testing.T) {
	server, roots := newSMTPTestServer(t, "tls", 250)
	config := validSMTPConfig()
	config.Port = server.port
	sender, err := newSMTPWithRoots(config, roots)
	if err != nil {
		t.Fatal(err)
	}
	delivery := testDelivery(domain.SMTP)
	delivery.Submission.Email = "иван@пример.рф"
	result := sender.Send(context.Background(), delivery)
	if !result.Success || result.Permanent || result.Code != "smtp_sent" {
		t.Fatalf("unexpected SMTP result: %+v", result)
	}
	message := <-server.messages
	parts := strings.SplitN(message, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatalf("message has no MIME body separator: %q", message)
	}
	if strings.Contains(strings.ToLower(parts[0]), "reply-to") {
		t.Fatalf("non-ASCII address produced a Reply-To header: %q", parts[0])
	}
	for i := 0; i < len(parts[0]); i++ {
		if parts[0][i] >= 0x80 {
			t.Fatalf("header byte %d is not ASCII: %q", i, parts[0])
		}
	}
	body, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(parts[1], "\r\n", ""))
	if err != nil {
		t.Fatalf("decode MIME body: %v", err)
	}
	if !strings.Contains(string(body), "Email: иван@пример.рф") {
		t.Fatalf("decoded body lacks the address: %q", body)
	}
	// The test server does not advertise SMTPUTF8, so this pins that the
	// envelope sender is plain and nothing in the message requires the extension.
	if mailFrom := <-server.mailFrom; strings.Contains(strings.ToUpper(mailFrom), "SMTPUTF8") {
		t.Fatalf("MAIL FROM requests SMTPUTF8: %q", mailFrom)
	}
}

func TestSMTPUsesVerifiedSTARTTLSBeforeAuthenticating(t *testing.T) {
	server, roots := newSMTPTestServer(t, "starttls", 250)
	config := validSMTPConfig()
	config.TLSMode = "starttls"
	config.Port = server.port
	sender, err := newSMTPWithRoots(config, roots)
	if err != nil {
		t.Fatal(err)
	}
	result := sender.Send(context.Background(), testDelivery(domain.SMTP))
	if !result.Success {
		t.Fatalf("STARTTLS delivery failed: %+v", result)
	}
	select {
	case auth := <-server.auth:
		if auth != "\x00smtp-user\x00smtp-password" {
			t.Fatalf("unexpected SMTP authentication payload: %q", auth)
		}
	case <-time.After(time.Second):
		t.Fatal("SMTP server did not receive authentication after TLS")
	}
}

func TestSMTPUsesAdvertisedLOGINAfterTLS(t *testing.T) {
	server, roots := newSMTPTestServerWithAuth(t, "tls", 250, []string{"LOGIN"}, nil, nil)
	config := validSMTPConfig()
	config.Port = server.port
	sender, err := newSMTPWithRoots(config, roots)
	if err != nil {
		t.Fatal(err)
	}
	result := sender.Send(context.Background(), testDelivery(domain.SMTP))
	if !result.Success {
		t.Fatalf("LOGIN delivery failed: %+v", result)
	}
	if auth := <-server.auth; auth != "smtp-user\x00smtp-password" {
		t.Fatalf("unexpected LOGIN authentication payload: %q", auth)
	}
	if command := <-server.authCommands; command != "AUTH LOGIN" {
		t.Fatalf("AUTH command = %q", command)
	}
}

func TestSMTPPrefersPLAINWhenBothMechanismsAreAdvertised(t *testing.T) {
	server, roots := newSMTPTestServerWithAuth(t, "tls", 250, []string{"login", "plain"}, nil, nil)
	config := validSMTPConfig()
	config.Port = server.port
	sender, err := newSMTPWithRoots(config, roots)
	if err != nil {
		t.Fatal(err)
	}
	result := sender.Send(context.Background(), testDelivery(domain.SMTP))
	if !result.Success {
		t.Fatalf("PLAIN delivery failed: %+v", result)
	}
	if command := <-server.authCommands; command != "AUTH PLAIN" {
		t.Fatalf("AUTH command = %q, want AUTH PLAIN", command)
	}
	if auth := <-server.auth; auth != "\x00smtp-user\x00smtp-password" {
		t.Fatalf("unexpected PLAIN authentication payload: %q", auth)
	}
}

func TestSMTPDoesNotAuthenticateWhenNoSupportedMechanismIsAdvertised(t *testing.T) {
	server, roots := newSMTPTestServerWithAuth(t, "tls", 250, []string{"CRAM-MD5"}, nil, nil)
	config := validSMTPConfig()
	config.Port = server.port
	sender, err := newSMTPWithRoots(config, roots)
	if err != nil {
		t.Fatal(err)
	}
	result := sender.Send(context.Background(), testDelivery(domain.SMTP))
	if result.Success || !result.Permanent || result.Code != "smtp_auth_unsupported" {
		t.Fatalf("unexpected result without supported auth: %+v", result)
	}
	select {
	case command := <-server.authCommands:
		t.Fatalf("client sent unsupported AUTH command %q", command)
	default:
	}
	select {
	case auth := <-server.auth:
		t.Fatalf("server received credentials without supported auth: %q", auth)
	default:
	}
}

func TestSMTPDoesNotFallBackAfterChosenMechanismIsRejected(t *testing.T) {
	server, roots := newSMTPTestServerWithAuth(t, "tls", 250, []string{"PLAIN", "LOGIN"}, nil, &smtpAuthScript{rejectCode: 535})
	config := validSMTPConfig()
	config.Port = server.port
	sender, err := newSMTPWithRoots(config, roots)
	if err != nil {
		t.Fatal(err)
	}
	result := sender.Send(context.Background(), testDelivery(domain.SMTP))
	if result.Success || !result.Permanent || result.Code != "smtp_permanent_rejection" {
		t.Fatalf("unexpected auth rejection result: %+v", result)
	}
	if command := <-server.authCommands; command != "AUTH PLAIN" {
		t.Fatalf("first AUTH command = %q", command)
	}
	select {
	case command := <-server.authCommands:
		t.Fatalf("client fell back after auth rejection with %q", command)
	default:
	}
	if strings.Contains(result.Code, config.Username) || strings.Contains(result.Code, config.Password) {
		t.Fatalf("credential leaked in result code %q", result.Code)
	}
}

func TestSMTPUsesPostSTARTTLSAuthenticationCapabilities(t *testing.T) {
	server, roots := newSMTPTestServerWithAuth(t, "starttls", 250, []string{"PLAIN"}, []string{"LOGIN"}, nil)
	config := validSMTPConfig()
	config.TLSMode = "starttls"
	config.Port = server.port
	sender, err := newSMTPWithRoots(config, roots)
	if err != nil {
		t.Fatal(err)
	}
	result := sender.Send(context.Background(), testDelivery(domain.SMTP))
	if !result.Success {
		t.Fatalf("STARTTLS LOGIN delivery failed: %+v", result)
	}
	if command := <-server.authCommands; command != "AUTH LOGIN" {
		t.Fatalf("AUTH command = %q, want post-TLS LOGIN", command)
	}
	if auth := <-server.auth; auth != "smtp-user\x00smtp-password" {
		t.Fatalf("unexpected LOGIN authentication payload: %q", auth)
	}
}

func TestSMTPClassifiesEHLOTransportFailureAsTransient(t *testing.T) {
	server, roots := newSMTPTestServerWithAuth(t, "ehlo-fail", 250, nil, nil, nil)
	config := validSMTPConfig()
	config.TLSMode = "starttls"
	config.Port = server.port
	sender, err := newSMTPWithRoots(config, roots)
	if err != nil {
		t.Fatal(err)
	}
	result := sender.Send(context.Background(), testDelivery(domain.SMTP))
	if result.Success || result.Permanent || result.Code != "smtp_transport_error" {
		t.Fatalf("unexpected result after EHLO transport failure: %+v", result)
	}
	select {
	case command := <-server.authCommands:
		t.Fatalf("client sent AUTH after EHLO transport failure: %q", command)
	default:
	}
}

func TestSMTPLoginAuthRequiresTLSHostAndOrderedChallenges(t *testing.T) {
	auth := loginAuthFor("smtp.example.test", "smtp-user", "smtp-password")
	for _, server := range []*smtp.ServerInfo{
		{Name: "smtp.example.test", Auth: []string{"LOGIN"}},
		{Name: "other.example.test", TLS: true, Auth: []string{"LOGIN"}},
		{Name: "smtp.example.test", TLS: true, Auth: []string{"PLAIN"}},
	} {
		if _, _, err := auth.Start(server); err == nil {
			t.Fatalf("LOGIN auth started for insecure or unsupported server: %+v", server)
		}
	}
	if mechanism, initial, err := auth.Start(&smtp.ServerInfo{Name: "smtp.example.test", TLS: true, Auth: []string{"login"}}); err != nil || mechanism != "LOGIN" || initial != nil {
		t.Fatalf("LOGIN start = %q, %q, %v", mechanism, initial, err)
	}
	if response, err := auth.Next([]byte("Password:"), true); err == nil || len(response) != 0 {
		t.Fatalf("out-of-order challenge response = %q, %v", response, err)
	}
	if response, err := auth.Next([]byte("Username:"), true); err != nil || string(response) != "smtp-user" {
		t.Fatalf("username response = %q, %v", response, err)
	}
	if response, err := auth.Next([]byte("Not the password prompt"), true); err == nil || len(response) != 0 {
		t.Fatalf("unexpected password challenge response = %q, %v", response, err)
	}
	if response, err := auth.Next([]byte("Password:"), true); err != nil || string(response) != "smtp-password" {
		t.Fatalf("password response = %q, %v", response, err)
	}
	if response, err := auth.Next(nil, false); err != nil || response != nil {
		t.Fatalf("completion response = %q, %v", response, err)
	}
}

func TestSMTPStartTLSFailureDoesNotSendCredentialsInPlaintext(t *testing.T) {
	server, _ := newSMTPTestServer(t, "no-starttls", 250)
	config := validSMTPConfig()
	config.TLSMode = "starttls"
	config.Port = server.port
	sender, err := NewSMTP(config)
	if err != nil {
		t.Fatal(err)
	}
	result := sender.Send(context.Background(), testDelivery(domain.SMTP))
	if result.Success || result.Code != "smtp_tls_required" || !result.Permanent {
		t.Fatalf("unexpected result when STARTTLS is unavailable: %+v", result)
	}
	select {
	case auth := <-server.auth:
		t.Fatalf("server received credentials without TLS: %q", auth)
	default:
	}
}

func TestSMTPClassifiesTemporaryAndPermanentProviderReplies(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      int
		permanent bool
	}{
		{name: "temporary", code: 450},
		{name: "permanent", code: 550, permanent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, roots := newSMTPTestServer(t, "tls", tc.code)
			config := validSMTPConfig()
			config.Port = server.port
			sender, err := newSMTPWithRoots(config, roots)
			if err != nil {
				t.Fatal(err)
			}
			result := sender.Send(context.Background(), testDelivery(domain.SMTP))
			if result.Success || result.Permanent != tc.permanent {
				t.Fatalf("unexpected classification: %+v", result)
			}
		})
	}
}

func TestNewTelegramRejectsUnsafeConfiguration(t *testing.T) {
	tests := []struct {
		name string
		edit func(*TelegramConfig)
	}{
		{name: "empty token", edit: func(c *TelegramConfig) { c.Token = "" }},
		{name: "invalid chat", edit: func(c *TelegramConfig) { c.ChatID = "chat\r\nX-Injected: yes" }},
		{name: "proxy credentials in URL", edit: func(c *TelegramConfig) { c.ProxyURL = "http://user:pass@proxy.example.test:8080" }},
		{name: "colon in proxy username", edit: func(c *TelegramConfig) { c.ProxyUsername = "proxy:user" }},
		{name: "oversized proxy username", edit: func(c *TelegramConfig) { c.ProxyUsername = strings.Repeat("u", 257) }},
		{name: "oversized proxy password", edit: func(c *TelegramConfig) { c.ProxyPassword = strings.Repeat("p", 257) }},
		{name: "proxy has no fixed port", edit: func(c *TelegramConfig) { c.ProxyURL = "http://proxy.example.test" }},
		{name: "proxy auth missing", edit: func(c *TelegramConfig) { c.ProxyPassword = "" }},
		{name: "proxy over TLS unsupported", edit: func(c *TelegramConfig) { c.ProxyURL = "https://proxy.example.test:8080" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := validTelegramConfig("http://127.0.0.1:8080")
			tt.edit(&config)
			if _, err := NewTelegram(config); err == nil {
				t.Fatal("NewTelegram accepted invalid configuration")
			}
		})
	}
}

func TestTelegramUsesFixedAuthenticatedCONNECTAndSendsNoPII(t *testing.T) {
	origin, roots := telegramOrigin(t, http.StatusOK, `{"ok":true,"result":{"message_id":812}}`)
	proxy := newConnectProxy(t, origin.Listener.Addr().String())
	config := validTelegramConfig(proxy.url)
	sender, err := newTelegramWithRoots(config, roots)
	if err != nil {
		t.Fatal(err)
	}

	result := sender.Send(context.Background(), testDelivery(domain.Telegram))
	if !result.Success || result.ProviderID != "812" || result.Permanent {
		t.Fatalf("unexpected result: %+v", result)
	}
	if target := <-proxy.targets; target != "api.telegram.org:443" {
		t.Fatalf("CONNECT target = %q", target)
	}
	if auth := <-proxy.auth; auth != "Basic "+base64.StdEncoding.EncodeToString([]byte("proxy-user:proxy-password")) {
		t.Fatalf("CONNECT proxy auth = %q", auth)
	}
	got := <-origin.requests
	if got.URL.Path != "/bot123456:dummy-token/sendMessage" {
		t.Fatalf("unexpected API path: %q", got.URL.Path)
	}
	var payload struct {
		ChatID string `json:"chat_id"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(got.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ChatID != "-100123456" {
		t.Fatalf("chat_id = %q", payload.ChatID)
	}
	for _, private := range []string{"Alice Private", "alice-private@example.test", "+15551234567", "secret message body"} {
		if strings.Contains(payload.Text, private) {
			t.Fatalf("Telegram text contains private content %q", private)
		}
	}
	for _, required := range []string{"Новое обращение", "sub-123", "Проверьте служебную почту", "+03:00"} {
		if !strings.Contains(payload.Text, required) {
			t.Errorf("Telegram text lacks %q: %q", required, payload.Text)
		}
	}
}

func TestTelegramCloseIsSafe(t *testing.T) {
	sender, err := NewTelegram(validTelegramConfig("http://127.0.0.1:8080"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Close(); err != nil {
		t.Fatalf("close Telegram transport: %v", err)
	}
}

func TestTelegramFloodControlReturnsBoundedRetryAfter(t *testing.T) {
	origin, roots := telegramOrigin(t, http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"too many requests","parameters":{"retry_after":37}}`)
	proxy := newConnectProxy(t, origin.Listener.Addr().String())
	sender, err := newTelegramWithRoots(validTelegramConfig(proxy.url), roots)
	if err != nil {
		t.Fatal(err)
	}

	result := sender.Send(context.Background(), testDelivery(domain.Telegram))
	if result.Success || result.Permanent || result.RetryAfter != 37*time.Second || result.Code != "telegram_rate_limited" {
		t.Fatalf("unexpected flood-control result: %+v", result)
	}
}

func TestTelegramDoesNotFollowRedirects(t *testing.T) {
	origin, roots := telegramOrigin(t, http.StatusTemporaryRedirect, `{"ok":true,"result":{"message_id":1}}`)
	proxy := newConnectProxy(t, origin.Listener.Addr().String())
	sender, err := newTelegramWithRoots(validTelegramConfig(proxy.url), roots)
	if err != nil {
		t.Fatal(err)
	}
	result := sender.Send(context.Background(), testDelivery(domain.Telegram))
	if result.Success || !result.Permanent || result.Code != "telegram_redirect_rejected" {
		t.Fatalf("unexpected redirect result: %+v", result)
	}
}

func TestTelegramClassifiesProviderFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		code      string
		permanent bool
	}{
		{name: "client rejection", status: http.StatusUnauthorized, body: `{"ok":false,"error_code":401}`, code: "telegram_rejected", permanent: true},
		{name: "provider failure", status: http.StatusServiceUnavailable, body: `{"ok":false,"error_code":503}`, code: "telegram_provider_error"},
		{name: "success without message id", status: http.StatusOK, body: `{"ok":true,"result":{}}`, code: "telegram_invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin, roots := telegramOrigin(t, tc.status, tc.body)
			proxy := newConnectProxy(t, origin.Listener.Addr().String())
			sender, err := newTelegramWithRoots(validTelegramConfig(proxy.url), roots)
			if err != nil {
				t.Fatal(err)
			}
			result := sender.Send(context.Background(), testDelivery(domain.Telegram))
			if result.Success || result.Code != tc.code || result.Permanent != tc.permanent {
				t.Fatalf("unexpected result: %+v", result)
			}
		})
	}
}

func TestTelegramRejectsUnsafeSubmissionID(t *testing.T) {
	sender, err := NewTelegram(validTelegramConfig("http://127.0.0.1:8080"))
	if err != nil {
		t.Fatal(err)
	}
	delivery := testDelivery(domain.Telegram)
	delivery.Submission.ID = "submission\nEmail: private@example.test"
	result := sender.Send(context.Background(), delivery)
	if result.Success || !result.Permanent || result.Code != "telegram_invalid_submission" {
		t.Fatalf("unexpected result for unsafe ID: %+v", result)
	}
}

func validSMTPConfig() SMTPConfig {
	return SMTPConfig{
		Host: "localhost", Port: 2525, TLSMode: "tls", Username: "smtp-user", Password: "smtp-password",
		From: "Feedback <feedback@example.test>", To: "Operations <ops@example.test>",
	}
}

func newSMTPWithRoots(config SMTPConfig, roots *x509.CertPool) (*SMTP, error) {
	return newSMTP(config, roots)
}

func newTelegramWithRoots(config TelegramConfig, roots *x509.CertPool) (*Telegram, error) {
	return newTelegram(config, roots)
}

func validTelegramConfig(proxyURL string) TelegramConfig {
	return TelegramConfig{
		Token: "123456:dummy-token", ChatID: "-100123456", ProxyURL: proxyURL,
		ProxyUsername: "proxy-user", ProxyPassword: "proxy-password",
	}
}

func testDelivery(channel domain.Channel) domain.Delivery {
	received := time.Date(2026, 10, 12, 10, 30, 0, 0, time.UTC)
	return domain.Delivery{Submission: domain.Submission{
		ID: "sub-123", Name: "Alice Private", Email: "alice-private@example.test", Phone: "+15551234567",
		Message: "secret message body", ReceivedAt: received,
	}, Job: domain.Job{Channel: channel}}
}

type recordedRequest struct {
	Header http.Header
	URL    url.URL
	Body   []byte
}

type telegramTestOrigin struct {
	Listener net.Listener
	requests chan recordedRequest
	server   *http.Server
}

func telegramOrigin(t *testing.T, status int, response string) (*telegramTestOrigin, *x509.CertPool) {
	t.Helper()
	certificate, roots := testCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	origin := &telegramTestOrigin{Listener: listener, requests: make(chan recordedRequest, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}
		origin.requests <- recordedRequest{Header: r.Header.Clone(), URL: *r.URL, Body: body}
		w.Header().Set("Content-Type", "application/json")
		if status >= 300 && status < 400 {
			w.Header().Set("Location", "https://redirect.example.test/capture")
		}
		w.WriteHeader(status)
		if _, err := io.WriteString(w, response); err != nil {
			return
		}
	})
	origin.server = &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	t.Cleanup(func() { closeQuietly(origin.server) })
	go func() {
		if err := origin.server.Serve(tlsListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve Telegram test origin: %v", err)
		}
	}()
	return origin, roots
}

func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "api.telegram.org test cert"},
		DNSNames:              []string{"api.telegram.org", "localhost"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return certificate, roots
}

type smtpTestServer struct {
	port         int
	messages     chan string
	auth         chan string
	authCommands chan string
	rcpt         chan string
	mailFrom     chan string
}

type smtpAuthScript struct {
	rejectCode int
}

func newSMTPTestServer(t *testing.T, mode string, finalCode int) (*smtpTestServer, *x509.CertPool) {
	postTLSAuth := []string(nil)
	if mode == "starttls" {
		postTLSAuth = []string{"PLAIN"}
	}
	return newSMTPTestServerWithAuth(t, mode, finalCode, []string{"PLAIN"}, postTLSAuth, nil)
}

func newSMTPTestServerWithAuth(t *testing.T, mode string, finalCode int, preTLSAuth, postTLSAuth []string, authScript *smtpAuthScript) (*smtpTestServer, *x509.CertPool) {
	t.Helper()
	certificate, roots := testCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &smtpTestServer{
		port:         smtpTestPort(t, listener),
		messages:     make(chan string, 1),
		auth:         make(chan string, 1),
		authCommands: make(chan string, 2),
		rcpt:         make(chan string, 1),
		mailFrom:     make(chan string, 1),
	}
	t.Cleanup(func() { closeQuietly(listener) })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go server.handle(t, conn, mode, finalCode, certificate, preTLSAuth, postTLSAuth, authScript)
		}
	}()
	return server, roots
}

func (s *smtpTestServer) handle(t *testing.T, conn net.Conn, mode string, finalCode int, certificate tls.Certificate, preTLSAuth, postTLSAuth []string, authScript *smtpAuthScript) {
	defer closeQuietly(conn)
	if mode == "tls" {
		conn = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	}
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	write := func(response string) bool {
		if _, err := io.WriteString(writer, response+"\r\n"); err != nil {
			return false
		}
		return writer.Flush() == nil
	}
	if !write("220 smtp.example.test ready") {
		return
	}
	authCapabilities := preTLSAuth
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command := strings.TrimSpace(line)
		fields := strings.Fields(command)
		if len(fields) == 0 {
			return
		}
		switch strings.ToUpper(fields[0]) {
		case "EHLO", "HELO":
			if mode == "ehlo-fail" {
				return
			}
			if mode == "no-starttls" {
				if !write("250 smtp.example.test") {
					return
				}
			} else {
				if !write("250-smtp.example.test") {
					return
				}
				if mode == "starttls" && !write("250-STARTTLS") {
					return
				}
				if !writeAuthExtension(write, authCapabilities) {
					return
				}
			}
		case "STARTTLS":
			if mode != "starttls" || !write("220 begin TLS") {
				return
			}
			secured := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
			conn = secured
			reader = bufio.NewReader(conn)
			writer = bufio.NewWriter(conn)
			mode = "starttls-after"
			authCapabilities = postTLSAuth
		case "AUTH":
			s.authCommands <- strings.ToUpper(strings.Join(fields[:2], " "))
			if len(fields) >= 2 && strings.EqualFold(fields[1], "PLAIN") && containsMechanism(authCapabilities, "PLAIN") {
				if len(fields) != 3 {
					if !write("501 invalid PLAIN response") {
						return
					}
					continue
				}
				decoded, decodeErr := base64.StdEncoding.DecodeString(fields[2])
				if decodeErr == nil {
					if authScript != nil && authScript.rejectCode > 0 {
						s.auth <- string(decoded)
						if !write(fmt.Sprintf("%d authentication rejected", authScript.rejectCode)) {
							return
						}
						continue
					}
					s.auth <- string(decoded)
				}
				if !write("235 authenticated") {
					return
				}
				continue
			}
			if len(fields) == 2 && strings.EqualFold(fields[1], "LOGIN") && containsMechanism(authCapabilities, "LOGIN") {
				if !write("334 " + base64.StdEncoding.EncodeToString([]byte("Username:"))) {
					return
				}
				username, readErr := reader.ReadString('\n')
				if readErr != nil {
					return
				}
				if !write("334 " + base64.StdEncoding.EncodeToString([]byte("Password:"))) {
					return
				}
				password, readErr := reader.ReadString('\n')
				if readErr != nil {
					return
				}
				userBytes, userErr := base64.StdEncoding.DecodeString(strings.TrimSpace(username))
				passBytes, passErr := base64.StdEncoding.DecodeString(strings.TrimSpace(password))
				if userErr != nil || passErr != nil {
					if !write("501 invalid LOGIN response") {
						return
					}
					continue
				}
				s.auth <- string(userBytes) + "\x00" + string(passBytes)
				code := 235
				if authScript != nil && authScript.rejectCode > 0 {
					code = authScript.rejectCode
				}
				if !write(fmt.Sprintf("%d authenticated", code)) {
					return
				}
				continue
			}
			if !write("504 unsupported authentication mechanism") {
				return
			}
		case "MAIL":
			select {
			case s.mailFrom <- command:
			default:
			}
			if !write("250 sender accepted") {
				return
			}
		case "RCPT":
			if len(fields) > 1 {
				s.rcpt <- strings.TrimPrefix(fields[1], "TO:")
			}
			if finalCode >= 400 {
				if !write(fmt.Sprintf("%d recipient rejected", finalCode)) {
					return
				}
				return
			}
			if !write("250 recipient accepted") {
				return
			}
		case "DATA":
			if !write("354 continue") {
				return
			}
			var message strings.Builder
			for {
				part, readErr := reader.ReadString('\n')
				if readErr != nil {
					return
				}
				if strings.TrimSpace(part) == "." {
					break
				}
				if _, err := message.WriteString(part); err != nil {
					return
				}
			}
			s.messages <- message.String()
			if !write("250 accepted") {
				return
			}
			return
		case "QUIT":
			if !write("221 bye") {
				return
			}
			return
		default:
			if !write("250 ok") {
				return
			}
		}
	}
}

func writeAuthExtension(write func(string) bool, mechanisms []string) bool {
	if len(mechanisms) == 0 {
		return write("250 smtp.example.test")
	}
	return write("250 AUTH " + strings.Join(mechanisms, " "))
}

func containsMechanism(mechanisms []string, wanted string) bool {
	for _, mechanism := range mechanisms {
		if strings.EqualFold(mechanism, wanted) {
			return true
		}
	}
	return false
}

func smtpTestPort(t *testing.T, listener net.Listener) int {
	t.Helper()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("SMTP test listener has address type %T", listener.Addr())
	}
	return address.Port
}

type connectProxy struct {
	url     string
	targets chan string
	auth    chan string
}

func newConnectProxy(t *testing.T, upstream string) *connectProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &connectProxy{url: "http://" + listener.Addr().String(), targets: make(chan string, 1), auth: make(chan string, 1)}
	t.Cleanup(func() { closeQuietly(listener) })
	go func() {
		for {
			client, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go proxy.handle(client, upstream)
		}
	}()
	return proxy
}

func (p *connectProxy) handle(client net.Conn, upstream string) {
	defer closeQuietly(client)
	reader := textproto.NewReader(bufioNewReader(client))
	line, err := reader.ReadLine()
	if err != nil {
		return
	}
	parts := strings.Fields(line)
	if len(parts) != 3 || parts[0] != http.MethodConnect {
		return
	}
	p.targets <- parts[1]
	header, err := reader.ReadMIMEHeader()
	if err != nil {
		return
	}
	p.auth <- header.Get("Proxy-Authorization")
	remote, err := net.DialTimeout("tcp", upstream, time.Second)
	if err != nil {
		return
	}
	defer closeQuietly(remote)
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	go func() {
		if _, err := io.Copy(remote, client); err != nil {
			return
		}
	}()
	if _, err := io.Copy(client, remote); err != nil {
		return
	}
}

func bufioNewReader(conn net.Conn) *bufio.Reader { return bufio.NewReader(conn) }

func TestSMTPTextBodyUsesCanonicalCRLFWithoutChangingSubmission(t *testing.T) {
	sender, err := NewSMTP(validSMTPConfig())
	if err != nil {
		t.Fatal(err)
	}
	delivery := testDelivery(domain.SMTP)
	delivery.Submission.Message = "first\nsecond\tline"
	message, err := sender.message(delivery.Submission)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(message), "\r\n\r\n", 2)
	body, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(parts[1], "\r\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "first\r\nsecond\tline") {
		t.Fatal("MIME body has a noncanonical line break")
	}
	if delivery.Submission.Message != "first\nsecond\tline" {
		t.Fatal("stored submission was changed")
	}
}

type a8TransportObserver struct{}

func (a8TransportObserver) Message(_ context.Context, d domain.Delivery, b []byte) ([]byte, error) {
	return append([]byte("X-Umir-Fixture-Job-ID: "+d.Job.ID+"\r\nX-Umir-Fixture-Submission-ID: "+string(d.Submission.ID)+"\r\n"), b...), nil
}
func (a8TransportObserver) Request(_ context.Context, d domain.Delivery, r *http.Request) error {
	r.Header.Set("X-Umir-Fixture-Job-ID", d.Job.ID)
	r.Header.Set("X-Umir-Fixture-Submission-ID", string(d.Submission.ID))
	return nil
}
func TestA8RealSMTPAndTelegramTransportObserver(t *testing.T) {
	jobID := "11111111-1111-4111-8111-111111111111"
	id := domain.SubmissionID("22222222-2222-4222-8222-222222222222")
	sink, roots := newSMTPTestServer(t, "starttls", 250)
	cfg := validSMTPConfig()
	cfg.Host = "localhost"
	cfg.Port = sink.port
	cfg.TLSMode = "starttls"
	sender, e := NewSMTPWithOptions(cfg, SMTPOptions{RootCAs: roots, Observer: a8TransportObserver{}})
	if e != nil {
		t.Fatal(e)
	}
	d := testDelivery(domain.SMTP)
	d.Job.ID = jobID
	d.Submission.ID = id
	d.Submission.Namespace = domain.Namespace{Kind: "app", UserID: "33333333-3333-4333-8333-333333333333", OrgID: "44444444-4444-4444-8444-444444444444", TenantID: "55555555-5555-4555-8555-555555555555"}
	if result := sender.Send(context.Background(), d); !result.Success {
		t.Fatalf("synthetic SMTP result %s", result.Code)
	}
	message := <-sink.messages
	for _, required := range []string{"X-Umir-Fixture-Job-ID: " + jobID, "X-Umir-Fixture-Submission-ID: " + string(id), "MIME-Version: 1.0"} {
		if !strings.Contains(message, required) {
			t.Fatal("missing rendered real SMTP correlation")
		}
	}
	origin, roots := telegramOrigin(t, 200, `{"ok":true,"result":{"message_id":12}}`)
	proxy := newConnectProxy(t, origin.Listener.Addr().String())
	telegram, e := NewTelegramWithOptions(validTelegramConfig(proxy.url), TelegramOptions{RootCAs: roots, Observer: a8TransportObserver{}})
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if e := telegram.Close(); e != nil {
			t.Error(e)
		}
	}()
	d.Job.Channel = domain.Telegram
	if result := telegram.Send(context.Background(), d); !result.Success {
		t.Fatalf("synthetic notification result %s", result.Code)
	}
	got := <-origin.requests
	if got.Header.Get("X-Umir-Fixture-Job-ID") != jobID || got.Header.Get("X-Umir-Fixture-Submission-ID") != string(id) {
		t.Fatal("missing real notification correlation")
	}
	for _, private := range []string{d.Submission.Name, d.Submission.Email, d.Submission.Phone, d.Submission.Message, d.Submission.Namespace.UserID, d.Submission.Namespace.OrgID, d.Submission.Namespace.TenantID} {
		if strings.Contains(string(got.Body), private) {
			t.Fatal("notification leaked private scope/content")
		}
	}
}
