package delivery

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

func TestRegistrationTemplateRequiresRealOperatorDetails(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*RegistrationMailConfig)
	}{
		{name: "missing operator name", edit: func(c *RegistrationMailConfig) { c.OperatorName = "" }},
		{name: "missing operator address", edit: func(c *RegistrationMailConfig) { c.OperatorAddress = " " }},
		{name: "missing sender name", edit: func(c *RegistrationMailConfig) { c.SenderName = "" }},
		{name: "placeholder operator name", edit: func(c *RegistrationMailConfig) { c.OperatorName = "[Наименование оператора]" }},
		{name: "dummy operator address", edit: func(c *RegistrationMailConfig) { c.OperatorAddress = "demo address" }},
		{name: "placeholder sender name", edit: func(c *RegistrationMailConfig) { c.SenderName = "Sender placeholder" }},
		{name: "Unicode control in operator name", edit: func(c *RegistrationMailConfig) { c.OperatorName = "ООО\u0085Оператор" }},
		{name: "non HTTPS privacy URL", edit: func(c *RegistrationMailConfig) { c.PrivacyURL = "http://privacy.rovenhod.ru/privacy" }},
		{name: "placeholder privacy URL", edit: func(c *RegistrationMailConfig) { c.PrivacyURL = "https://example.test/privacy" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := validRegistrationMailConfig()
			tc.edit(&config)
			if _, err := NewRegistrationTemplate(config); err == nil {
				t.Fatal("template accepted invalid operator mail configuration")
			}
		})
	}
}

func TestRegistrationTemplateRendersFixedRegistrationCopyAndActualMSKTimes(t *testing.T) {
	template, err := NewRegistrationTemplate(validRegistrationMailConfig())
	if err != nil {
		t.Fatal(err)
	}
	mail := testCodeMail()
	rendered, err := template.Render(mail)
	if err != nil {
		t.Fatal(err)
	}
	if rendered.Subject != "Подтверждение регистрации в «Ровном ходе»" {
		t.Fatalf("unexpected subject: %q", rendered.Subject)
	}
	if rendered.LogoContentType != "image/png" || rendered.LogoContentID == "" || len(rendered.LogoPNG) == 0 {
		t.Fatalf("registration logo is incomplete: type=%q id=%q bytes=%d", rendered.LogoContentType, rendered.LogoContentID, len(rendered.LogoPNG))
	}
	for _, required := range []string{
		"Подтвердите регистрацию",
		"012 034",
		"Запрос: 12.10.2026, 15:02 МСК.",
		"Срок действия кода истекает: 12.10.2026, 15:12:00 МСК.",
		"Если вы не запрашивали регистрацию, просто проигнорируйте это письмо.",
		"Никому не сообщайте код.",
		"ООО «Оператор»",
		"г. Москва, ул. Тестовая, д. 1",
	} {
		if !strings.Contains(rendered.Text, required) {
			t.Errorf("plain text lacks %q: %q", required, rendered.Text)
		}
		if !strings.Contains(rendered.HTML, required) {
			t.Errorf("HTML lacks %q: %q", required, rendered.HTML)
		}
	}
	for _, forbidden := range []string{"Тема:", "Предпросмотр:"} {
		if strings.Contains(rendered.HTML, forbidden) || strings.Contains(rendered.Text, forbidden) {
			t.Errorf("email body repeats MIME metadata label %q", forbidden)
		}
	}
	if !strings.Contains(rendered.HTML, rendered.Preheader) {
		t.Fatal("hidden HTML preheader is missing")
	}
	for _, forbidden := range []string{"10 минут", "Код для входа", "Смените пароль", "[Наименование оператора]", "demo-login", "012 034"} {
		if strings.Contains(rendered.Subject, forbidden) || strings.Contains(rendered.Preheader, forbidden) {
			t.Errorf("subject/preheader contains forbidden content %q", forbidden)
		}
	}
	if !strings.Contains(rendered.HTML, "cid:"+rendered.LogoContentID) {
		t.Fatalf("HTML does not reference embedded logo %q", rendered.LogoContentID)
	}
	if strings.Contains(strings.ToLower(rendered.HTML), "http://") || strings.Contains(strings.ToLower(rendered.HTML), "https://") {
		t.Fatalf("default HTML has an unexpected external link: %q", rendered.HTML)
	}
	if !strings.Contains(rendered.HTML, `lang="ru"`) {
		t.Fatal("HTML does not set Russian language")
	}
}

func TestRegistrationTemplateAllowsOnlyFixedHTTPSPrivacyURL(t *testing.T) {
	config := validRegistrationMailConfig()
	config.PrivacyURL = "https://privacy.rovenhod.ru/privacy"
	template, err := NewRegistrationTemplate(config)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := template.Render(testCodeMail())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(rendered.HTML, config.PrivacyURL) != 1 || strings.Count(rendered.Text, config.PrivacyURL) != 1 {
		t.Fatalf("privacy URL must appear once in each alternative: html=%q text=%q", rendered.HTML, rendered.Text)
	}
}

func TestRegistrationTemplateRequiresTenMinuteChallengeLifetime(t *testing.T) {
	template, err := NewRegistrationTemplate(validRegistrationMailConfig())
	if err != nil {
		t.Fatal(err)
	}
	mail := testCodeMail()
	mail.ExpiresAt = mail.CreatedAt.Add(9*time.Minute + 59*time.Second)
	if _, err := template.Render(mail); err == nil {
		t.Fatal("template accepted a challenge with a shortened lifetime")
	}
	mail.ExpiresAt = mail.CreatedAt.Add(10 * time.Minute)
	if _, err := template.Render(mail); err != nil {
		t.Fatalf("template rejected the fixed challenge lifetime: %v", err)
	}
}

func TestRegistrationSMTPAcceptsChallengeDomainEmailAndRoundTripsMIME(t *testing.T) {
	transport, err := NewSMTP(validSMTPConfig())
	if err != nil {
		t.Fatal(err)
	}
	sender, err := NewRegistrationSMTP(transport, validRegistrationMailConfig())
	if err != nil {
		t.Fatal(err)
	}
	codeMail := testCodeMail()
	codeMail.Email = "alice@пример.рф"
	message, err := sender.message(codeMail)
	if err != nil {
		t.Fatalf("registration MIME rejected a domain-valid normalized email: %v", err)
	}
	parsed, err := mail.ReadMessage(strings.NewReader(string(message)))
	if err != nil {
		t.Fatalf("parse registration MIME: %v", err)
	}
	address, err := mail.ParseAddress(parsed.Header.Get("To"))
	if err != nil || address.Address != codeMail.Email {
		t.Fatalf("MIME recipient round trip = %+v, error = %v", address, err)
	}
}

func TestRegistrationSMTPUsesChallengeRecipientAndRelatedAlternativeMIME(t *testing.T) {
	server, roots := newSMTPTestServer(t, "tls", 250)
	config := validSMTPConfig()
	config.Port = server.port
	intake, err := newSMTPWithRoots(config, roots)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := NewRegistrationSMTP(intake, validRegistrationMailConfig())
	if err != nil {
		t.Fatal(err)
	}
	codeMail := testCodeMail()
	sender.now = func() time.Time { return codeMail.CreatedAt.Add(time.Second) }
	result := sender.SendOTP(context.Background(), codeMail)
	if !result.Success || result.Code != "smtp_sent" {
		t.Fatalf("unexpected registration delivery: %+v", result)
	}
	message := <-server.messages
	if recipient := <-server.rcpt; recipient != "<alice@example.test>" {
		t.Fatalf("SMTP envelope recipient = %q", recipient)
	}
	for _, required := range []string{
		`To: <alice@example.test>`,
		"multipart/related",
		"multipart/alternative",
		"text/plain; charset=utf-8",
		"text/html; charset=utf-8",
		"image/png",
	} {
		if !strings.Contains(message, required) {
			t.Errorf("MIME message lacks %q: %q", required, message)
		}
	}
	for _, forbidden := range []string{"ops@example.test", "Bcc:", "012034", "https://", "<script"} {
		if strings.Contains(message, forbidden) {
			t.Errorf("MIME message contains forbidden content %q", forbidden)
		}
	}
	parsed, err := mail.ReadMessage(strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Header.Get("To") != "<alice@example.test>" {
		t.Fatalf("registration recipient = %q", parsed.Header.Get("To"))
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	if subject != "Подтверждение регистрации в «Ровном ходе»" {
		t.Fatalf("decoded registration subject = %q", subject)
	}
	mediaType, relatedParams, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/related" {
		t.Fatalf("top-level MIME type = %q, error = %v", mediaType, err)
	}
	related := multipart.NewReader(parsed.Body, relatedParams["boundary"])
	alternativePart, err := related.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	altType, altParams, err := mime.ParseMediaType(alternativePart.Header.Get("Content-Type"))
	if err != nil || altType != "multipart/alternative" {
		t.Fatalf("nested MIME type = %q, error = %v", altType, err)
	}
	alternative := multipart.NewReader(alternativePart, altParams["boundary"])
	for _, wantedType := range []string{"text/plain", "text/html"} {
		part, err := alternative.NextPart()
		if err != nil {
			t.Fatalf("read %s alternative: %v", wantedType, err)
		}
		partType, _, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil || partType != wantedType {
			t.Fatalf("alternative type = %q, want %q, error = %v", partType, wantedType, err)
		}
		decoded, err := io.ReadAll(quotedprintable.NewReader(part))
		if err != nil {
			t.Fatalf("decode %s alternative: %v", wantedType, err)
		}
		if !strings.Contains(string(decoded), "012 034") || !strings.Contains(string(decoded), "15:12:00 МСК") {
			t.Errorf("%s alternative lacks grouped code or original expiry: %q", wantedType, decoded)
		}
	}
	logoPart, err := related.NextPart()
	if err != nil {
		t.Fatalf("read inline logo: %v", err)
	}
	if got := logoPart.Header.Get("Content-ID"); got != "<registration-logo@umir-feedback>" {
		t.Fatalf("logo content ID = %q", got)
	}
	logoBase64, err := io.ReadAll(logoPart)
	if err != nil || !strings.HasPrefix(string(logoBase64), "iVBORw0KGgo") {
		t.Fatalf("inline logo is not valid base64 PNG, error = %v", err)
	}
}

func TestRegistrationSMTPRejectsInvalidChallengeRecipientWithoutConnecting(t *testing.T) {
	server, roots := newSMTPTestServer(t, "tls", 250)
	config := validSMTPConfig()
	config.Port = server.port
	intake, err := newSMTPWithRoots(config, roots)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := NewRegistrationSMTP(intake, validRegistrationMailConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, recipient := range []string{"alice@example.test\r\nBcc: victim@example.test", "Alice <alice@example.test>", "alice@example.test,other@example.test"} {
		mail := testCodeMail()
		mail.Email = recipient
		sender.now = func() time.Time { return mail.CreatedAt.Add(time.Second) }
		result := sender.SendOTP(context.Background(), mail)
		if result.Success || !result.Permanent || result.Code != "smtp_invalid_registration_mail" {
			t.Errorf("recipient %q result = %+v", recipient, result)
		}
	}
	select {
	case message := <-server.messages:
		t.Fatalf("invalid challenge recipient connected and sent %q", message)
	default:
	}
}

func TestRegistrationSMTPKeepsConfiguredTransportTLSMode(t *testing.T) {
	server, roots := newSMTPTestServer(t, "starttls", 250)
	config := validSMTPConfig()
	config.Port = server.port
	config.TLSMode = "starttls"
	intake, err := newSMTPWithRoots(config, roots)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := NewRegistrationSMTP(intake, validRegistrationMailConfig())
	if err != nil {
		t.Fatal(err)
	}
	codeMail := testCodeMail()
	sender.now = func() time.Time { return codeMail.CreatedAt.Add(time.Second) }
	result := sender.SendOTP(context.Background(), codeMail)
	if !result.Success {
		t.Fatalf("STARTTLS registration delivery failed: %+v", result)
	}
	select {
	case auth := <-server.auth:
		if auth != "\x00smtp-user\x00smtp-password" {
			t.Fatalf("unexpected SMTP authentication payload: %q", auth)
		}
	case <-time.After(time.Second):
		t.Fatal("registration SMTP did not authenticate after TLS")
	}
}

func TestRegistrationTemplateDoesNotUseCodeAsMessageHeader(t *testing.T) {
	template, err := NewRegistrationTemplate(validRegistrationMailConfig())
	if err != nil {
		t.Fatal(err)
	}
	mail := testCodeMail()
	mail.Code = "\r\nBcc: victim@example.test"
	if _, err := template.Render(mail); err == nil {
		t.Fatal("template accepted an invalid code")
	}
}

func TestRegistrationSMTPDoesNotSendExpiredCode(t *testing.T) {
	server, roots := newSMTPTestServer(t, "tls", 250)
	config := validSMTPConfig()
	config.Port = server.port
	intake, err := newSMTPWithRoots(config, roots)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := NewRegistrationSMTP(intake, validRegistrationMailConfig())
	if err != nil {
		t.Fatal(err)
	}
	codeMail := testCodeMail()
	sender.now = func() time.Time { return codeMail.ExpiresAt }
	result := sender.SendOTP(context.Background(), codeMail)
	if result.Success || !result.Permanent || result.Code != "smtp_registration_expired" {
		t.Fatalf("expired registration code result = %+v", result)
	}
	select {
	case message := <-server.messages:
		t.Fatalf("expired code was sent: %q", message)
	default:
	}
}

func validRegistrationMailConfig() RegistrationMailConfig {
	return RegistrationMailConfig{
		OperatorName: "ООО «Оператор»", OperatorAddress: "г. Москва, ул. Тестовая, д. 1", SenderName: "Ровный ход",
	}
}

func testCodeMail() domain.CodeMail {
	return domain.CodeMail{
		ChallengeID: "challenge-123", IssuanceID: "issue-123", Email: "alice@example.test", Code: "012034",
		CreatedAt: time.Date(2026, 10, 12, 15, 2, 0, 0, moscowLocation()),
		ExpiresAt: time.Date(2026, 10, 12, 15, 12, 0, 0, moscowLocation()),
	}
}

func TestRegistrationTemplateMobilePreviewContainsNoFixedWidthOverflow(t *testing.T) {
	template, err := NewRegistrationTemplate(validRegistrationMailConfig())
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := template.Render(testCodeMail())
	if err != nil {
		t.Fatal(err)
	}
	for _, width := range []int{600, 390, 320} {
		if strings.Contains(rendered.HTML, fmt.Sprintf("width:%dpx", width)) && width != 600 {
			t.Errorf("HTML hardcodes a viewport width of %dpx", width)
		}
	}
	if strings.Contains(rendered.HTML, "@font-face") || strings.Contains(rendered.HTML, "fonts.googleapis.com") || strings.Contains(rendered.HTML, "@media") {
		t.Fatal("email depends on external web fonts or viewport breakpoints")
	}
}

func TestRegistrationLogoIsValidPNG(t *testing.T) {
	template, err := NewRegistrationTemplate(validRegistrationMailConfig())
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := template.Render(testCodeMail())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(base64.StdEncoding.EncodeToString(rendered.LogoPNG), "iVBORw0KGgo") {
		t.Fatal("embedded registration mark is not a PNG")
	}
}
