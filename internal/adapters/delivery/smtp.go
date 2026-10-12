package delivery

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

const (
	connectTimeout = 5 * time.Second
	sessionTimeout = 15 * time.Second
)

var (
	messageIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	domainPattern    = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$`)
	hostPattern      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)
)

type SMTPConfig struct {
	Host     string
	Port     int
	TLSMode  string
	Username string
	Password string
	From     string
	To       string
}

type SMTP struct {
	host     string
	port     int
	tlsMode  string
	username string
	password string
	from     mail.Address
	to       mail.Address
	fromAddr string
	toAddr   string
	domain   string
	roots    *x509.CertPool
}

func NewSMTP(config SMTPConfig) (*SMTP, error) {
	return newSMTP(config, nil)
}

func newSMTP(config SMTPConfig, roots *x509.CertPool) (*SMTP, error) {
	if !validSMTPHost(config.Host) {
		return nil, errors.New("invalid FEEDBACK_SMTP_HOST")
	}
	if config.Port < 1 || config.Port > 65535 {
		return nil, errors.New("invalid FEEDBACK_SMTP_PORT")
	}
	if config.TLSMode != "starttls" && config.TLSMode != "tls" {
		return nil, errors.New("invalid FEEDBACK_SMTP_TLS_MODE")
	}
	if strings.TrimSpace(config.Username) == "" || strings.TrimSpace(config.Password) == "" || strings.ContainsAny(config.Username+config.Password, "\r\n\x00") {
		return nil, errors.New("invalid FEEDBACK_SMTP_USERNAME or FEEDBACK_SMTP_PASSWORD")
	}
	from, fromErr := parseMailbox(config.From)
	if fromErr != nil {
		return nil, errors.New("invalid FEEDBACK_MAIL_FROM")
	}
	to, toErr := parseMailbox(config.To)
	if toErr != nil {
		return nil, errors.New("invalid FEEDBACK_MAIL_TO")
	}
	_, domainName, ok := strings.Cut(from.Address, "@")
	if !ok || !domainPattern.MatchString(domainName) {
		return nil, errors.New("invalid FEEDBACK_MAIL_FROM")
	}
	return &SMTP{
		host: config.Host, port: config.Port, tlsMode: config.TLSMode,
		username: config.Username, password: config.Password,
		from: from, to: to, fromAddr: from.Address, toAddr: to.Address,
		domain: domainName, roots: roots,
	}, nil
}

func validSMTPHost(host string) bool {
	if host == "" || strings.ContainsAny(host, " \t\r\n/\\") {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	return hostPattern.MatchString(host)
}

func parseMailbox(value string) (mail.Address, error) {
	if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n\x00") {
		return mail.Address{}, errors.New("invalid mailbox")
	}
	address, err := mail.ParseAddress(value)
	if err != nil || strings.ContainsAny(address.Address, "\r\n\x00") {
		return mail.Address{}, errors.New("invalid mailbox")
	}
	if !strings.Contains(address.Address, "@") {
		return mail.Address{}, errors.New("invalid mailbox")
	}
	return *address, nil
}

func (s *SMTP) Send(ctx context.Context, delivery domain.Delivery) domain.DeliveryResult {
	if delivery.Job.Channel != domain.SMTP {
		return permanentSMTP("smtp_invalid_channel")
	}
	message, err := s.message(delivery.Submission)
	if err != nil {
		return permanentSMTP("smtp_invalid_submission")
	}
	return s.send(ctx, s.toAddr, message)
}

func (s *SMTP) send(ctx context.Context, to string, message []byte) domain.DeliveryResult {
	sessionCtx, cancel := context.WithTimeout(ctx, sessionTimeout)
	defer cancel()
	dialCtx, dialCancel := context.WithTimeout(sessionCtx, connectTimeout)
	conn, err := (&net.Dialer{Timeout: connectTimeout}).DialContext(dialCtx, "tcp", net.JoinHostPort(s.host, strconv.Itoa(s.port)))
	dialCancel()
	if err != nil {
		return transientSMTP("smtp_transport_error")
	}
	rawConn := conn
	defer closeQuietly(conn)
	if deadline, ok := sessionCtx.Deadline(); ok {
		if err := rawConn.SetDeadline(deadline); err != nil {
			return transientSMTP("smtp_transport_error")
		}
	}
	stopCancel := context.AfterFunc(sessionCtx, func() {
		if err := rawConn.SetDeadline(time.Now()); err != nil {
			return
		}
	})
	defer stopCancel()

	if s.tlsMode == "tls" {
		secured := tls.Client(conn, s.tlsConfig())
		if err := secured.HandshakeContext(sessionCtx); err != nil {
			return transientSMTP("smtp_tls_error")
		}
		conn = secured
	}
	client, err := smtp.NewClient(conn, s.host)
	if err != nil {
		return classifySMTP(err)
	}
	defer closeQuietly(client)
	if err := client.Hello("localhost"); err != nil {
		return classifySMTP(err)
	}
	if s.tlsMode == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return permanentSMTP("smtp_tls_required")
		}
		if err := client.StartTLS(s.tlsConfig()); err != nil {
			return classifySMTP(err)
		}
	}
	auth, ok := selectSMTPAuth(client, s.host, s.username, s.password)
	if !ok {
		return permanentSMTP("smtp_auth_unsupported")
	}
	if err := client.Auth(auth); err != nil {
		return classifySMTP(err)
	}
	if err := client.Mail(s.fromAddr); err != nil {
		return classifySMTP(err)
	}
	if err := client.Rcpt(to); err != nil {
		return classifySMTP(err)
	}
	writer, err := client.Data()
	if err != nil {
		return classifySMTP(err)
	}
	if _, err := writer.Write(message); err != nil {
		if closeErr := writer.Close(); closeErr != nil {
			return transientSMTP("smtp_transport_error")
		}
		return transientSMTP("smtp_transport_error")
	}
	if err := writer.Close(); err != nil {
		return classifySMTP(err)
	}
	return domain.DeliveryResult{Success: true, Code: "smtp_sent"}
}

func selectSMTPAuth(client *smtp.Client, host, username, password string) (smtp.Auth, bool) {
	if ok, mechanisms := client.Extension("AUTH"); ok {
		for _, mechanism := range strings.Fields(mechanisms) {
			if strings.EqualFold(mechanism, "PLAIN") {
				return smtp.PlainAuth("", username, password, host), true
			}
		}
		for _, mechanism := range strings.Fields(mechanisms) {
			if strings.EqualFold(mechanism, "LOGIN") {
				return loginAuthFor(host, username, password), true
			}
		}
	}
	return nil, false
}

func (s *SMTP) tlsConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, ServerName: s.host, RootCAs: s.roots}
}

func (s *SMTP) message(submission domain.Submission) ([]byte, error) {
	id := string(submission.ID)
	if !messageIDPattern.MatchString(id) || submission.ReceivedAt.IsZero() || !utf8.ValidString(submission.Name+submission.Email+submission.Phone+submission.Message) {
		return nil, errors.New("invalid submission")
	}
	replyTo, err := parseMailbox(submission.Email)
	if err != nil {
		return nil, err
	}
	messageID := "<" + id + "@" + s.domain + ">"
	var body strings.Builder
	if _, err := fmt.Fprintf(&body, "Submission ID: %s\r\nReceived at: %s\r\nName: %s\r\nEmail: %s\r\n", id, submission.ReceivedAt.In(moscowLocation()).Format(time.RFC3339), submission.Name, submission.Email); err != nil {
		return nil, err
	}
	if submission.Phone != "" {
		if _, err := fmt.Fprintf(&body, "Phone: %s\r\n", submission.Phone); err != nil {
			return nil, err
		}
	}
	if _, err := fmt.Fprintf(&body, "\r\n%s\r\n", strings.ReplaceAll(submission.Message, "\n", "\r\n")); err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(body.String()))
	var wrapped strings.Builder
	for len(encoded) > 76 {
		if _, err := wrapped.WriteString(encoded[:76]); err != nil {
			return nil, err
		}
		if _, err := wrapped.WriteString("\r\n"); err != nil {
			return nil, err
		}
		encoded = encoded[76:]
	}
	if _, err := wrapped.WriteString(encoded); err != nil {
		return nil, err
	}
	if _, err := wrapped.WriteString("\r\n"); err != nil {
		return nil, err
	}
	message := fmt.Sprintf("From: %s\r\nTo: %s\r\nReply-To: %s\r\nSubject: New feedback: %s\r\nMessage-ID: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n%s", s.from.String(), s.to.String(), replyTo.String(), id, messageID, wrapped.String())
	return []byte(message), nil
}

func classifySMTP(err error) domain.DeliveryResult {
	var protocolErr *textproto.Error
	if errors.As(err, &protocolErr) {
		if protocolErr.Code >= 400 && protocolErr.Code < 500 {
			return transientSMTP("smtp_temporary_rejection")
		}
		if protocolErr.Code >= 500 && protocolErr.Code < 600 {
			return permanentSMTP("smtp_permanent_rejection")
		}
	}
	return transientSMTP("smtp_transport_error")
}

func transientSMTP(code string) domain.DeliveryResult {
	return domain.DeliveryResult{Code: code}
}

func permanentSMTP(code string) domain.DeliveryResult {
	return domain.DeliveryResult{Permanent: true, Code: code}
}

func moscowLocation() *time.Location {
	return time.FixedZone("MSK", 3*60*60)
}
