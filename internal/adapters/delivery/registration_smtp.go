package delivery

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

type RegistrationSMTP struct {
	smtp     *SMTP
	template *RegistrationTemplate
	now      func() time.Time
}

func NewRegistrationSMTP(existing *SMTP, mailConfig RegistrationMailConfig) (*RegistrationSMTP, error) {
	if existing == nil {
		return nil, errors.New("registration SMTP requires an existing SMTP transport")
	}
	template, err := NewRegistrationTemplate(mailConfig)
	if err != nil {
		return nil, err
	}
	return &RegistrationSMTP{smtp: existing, template: template, now: time.Now}, nil
}

func (s *RegistrationSMTP) SendOTP(ctx context.Context, codeMail domain.CodeMail) domain.DeliveryResult {
	if s == nil || s.smtp == nil || s.template == nil || s.now == nil {
		return permanentSMTP("smtp_invalid_registration_mail")
	}
	if !s.now().Before(codeMail.ExpiresAt) {
		return permanentSMTP("smtp_registration_expired")
	}
	message, err := s.message(codeMail)
	if err != nil {
		return permanentSMTP("smtp_invalid_registration_mail")
	}
	return s.smtp.send(ctx, codeMail.Email, message)
}

func (s *RegistrationSMTP) message(codeMail domain.CodeMail) ([]byte, error) {
	rendered, err := s.template.Render(codeMail)
	if err != nil {
		return nil, err
	}
	var alternative bytes.Buffer
	alt := multipart.NewWriter(&alternative)
	if err := writeRegistrationTextPart(alt, "text/plain; charset=utf-8", rendered.Text); err != nil {
		return nil, err
	}
	if err := writeRegistrationTextPart(alt, "text/html; charset=utf-8", rendered.HTML); err != nil {
		return nil, err
	}
	if err := alt.Close(); err != nil {
		return nil, err
	}

	var relatedBody bytes.Buffer
	related := multipart.NewWriter(&relatedBody)
	alternativeHeader := make(textproto.MIMEHeader)
	alternativeHeader.Set("Content-Type", fmt.Sprintf("multipart/alternative; boundary=%q", alt.Boundary()))
	alternativePart, err := related.CreatePart(alternativeHeader)
	if err != nil {
		return nil, err
	}
	if _, err := alternativePart.Write(alternative.Bytes()); err != nil {
		return nil, err
	}
	logoHeader := make(textproto.MIMEHeader)
	logoHeader.Set("Content-Type", rendered.LogoContentType)
	logoHeader.Set("Content-Transfer-Encoding", "base64")
	logoHeader.Set("Content-ID", "<"+rendered.LogoContentID+">")
	logoHeader.Set("Content-Disposition", `inline; filename="registration-logo.png"`)
	logoPart, err := related.CreatePart(logoHeader)
	if err != nil {
		return nil, err
	}
	if err := writeMIMEBase64(logoPart, rendered.LogoPNG); err != nil {
		return nil, err
	}
	if err := related.Close(); err != nil {
		return nil, err
	}

	from := mail.Address{Name: s.template.config.SenderName, Address: s.smtp.from.Address}
	to := mail.Address{Address: codeMail.Email}
	messageID := "<registration." + string(codeMail.ChallengeID) + "." + string(codeMail.IssuanceID) + "@" + s.smtp.domain + ">"
	subject := mime.QEncoding.Encode("UTF-8", rendered.Subject)
	contentType := fmt.Sprintf(`multipart/related; type="multipart/alternative"; boundary=%q`, related.Boundary())
	var message bytes.Buffer
	if _, err := fmt.Fprintf(&message, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: %s\r\nMIME-Version: 1.0\r\nContent-Type: %s\r\n\r\n", from.String(), to.String(), subject, messageID, contentType); err != nil {
		return nil, err
	}
	if _, err := message.Write(relatedBody.Bytes()); err != nil {
		return nil, err
	}
	return message.Bytes(), nil
}

func writeRegistrationTextPart(writer *multipart.Writer, contentType, body string) error {
	header := make(textproto.MIMEHeader)
	header.Set("Content-Type", contentType)
	header.Set("Content-Transfer-Encoding", "quoted-printable")
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	qp := quotedprintable.NewWriter(part)
	if _, err := qp.Write([]byte(body)); err != nil {
		return err
	}
	return qp.Close()
}

func writeMIMEBase64(writer io.Writer, value []byte) error {
	encoded := base64.StdEncoding.EncodeToString(value)
	for len(encoded) > 76 {
		if _, err := writer.Write([]byte(encoded[:76] + "\r\n")); err != nil {
			return err
		}
		encoded = encoded[76:]
	}
	if _, err := writer.Write([]byte(encoded + "\r\n")); err != nil {
		return err
	}
	return nil
}
