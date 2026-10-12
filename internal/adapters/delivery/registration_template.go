package delivery

import (
	"bytes"
	"embed"
	"errors"
	"html/template"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

const registrationLogoContentID = "registration-logo@umir-feedback"

var (
	registrationCodePattern = regexp.MustCompile(`^[0-9]{6}$`)
	registrationIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	registrationMSK         = loadRegistrationMSK()
)

//go:embed templates/registration.html.tmpl assets/registration-logo@2x.png
var registrationAssets embed.FS

type RegistrationMailConfig struct {
	OperatorName    string
	OperatorAddress string
	SenderName      string
	PrivacyURL      string
}

type RegistrationMessage struct {
	Subject         string
	Preheader       string
	Text            string
	HTML            string
	LogoContentID   string
	LogoContentType string
	LogoPNG         []byte
}

type RegistrationTemplate struct {
	config RegistrationMailConfig
	tmpl   *template.Template
	logo   []byte
}

func NewRegistrationTemplate(config RegistrationMailConfig) (*RegistrationTemplate, error) {
	if !validOperatorDetail(config.OperatorName) || len(config.OperatorName) > 256 || !validOperatorDetail(config.OperatorAddress) || len(config.OperatorAddress) > 512 || !validOperatorDetail(config.SenderName) || len(config.SenderName) > 160 {
		return nil, errors.New("registration mail operator details are required")
	}
	if config.PrivacyURL != "" && !validPrivacyURL(config.PrivacyURL) {
		return nil, errors.New("invalid registration mail privacy URL")
	}
	markup, err := registrationAssets.ReadFile("templates/registration.html.tmpl")
	if err != nil {
		return nil, err
	}
	logo, err := registrationAssets.ReadFile("assets/registration-logo@2x.png")
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("registration").Parse(string(markup))
	if err != nil {
		return nil, err
	}
	return &RegistrationTemplate{config: config, tmpl: tmpl, logo: logo}, nil
}

func (t *RegistrationTemplate) Render(mail domain.CodeMail) (RegistrationMessage, error) {
	if t == nil || t.tmpl == nil || !validRegistrationTarget(mail.Email) || !registrationCodePattern.MatchString(mail.Code) || !validRegistrationID(string(mail.ChallengeID)) || !validRegistrationID(string(mail.IssuanceID)) || mail.CreatedAt.IsZero() || mail.ExpiresAt.IsZero() || mail.ExpiresAt.Sub(mail.CreatedAt) != 10*time.Minute {
		return RegistrationMessage{}, errors.New("invalid registration mail")
	}
	subject := "Подтверждение регистрации в «Ровном ходе»"
	preheader := "Подтвердите регистрацию по коду. Никому не сообщайте его; срок указан в письме."
	createdAt := formatRegistrationTime(mail.CreatedAt)
	expiresAt := formatRegistrationDeadline(mail.ExpiresAt)
	groupedCode := mail.Code[:3] + " " + mail.Code[3:]
	data := struct {
		Subject         string
		Preheader       string
		GroupedCode     string
		CreatedAt       string
		ExpiresAt       string
		OperatorName    string
		OperatorAddress string
		PrivacyURL      string
		LogoContentID   string
	}{
		Subject: subject, Preheader: preheader, GroupedCode: groupedCode, CreatedAt: createdAt, ExpiresAt: expiresAt,
		OperatorName: t.config.OperatorName, OperatorAddress: t.config.OperatorAddress, PrivacyURL: t.config.PrivacyURL,
		LogoContentID: registrationLogoContentID,
	}
	var htmlBody bytes.Buffer
	if err := t.tmpl.Execute(&htmlBody, data); err != nil {
		return RegistrationMessage{}, err
	}
	plainText := renderRegistrationText(data)
	return RegistrationMessage{
		Subject: subject, Preheader: preheader, Text: plainText, HTML: htmlBody.String(),
		LogoContentID: registrationLogoContentID, LogoContentType: "image/png", LogoPNG: append([]byte(nil), t.logo...),
	}, nil
}

func renderRegistrationText(data struct {
	Subject         string
	Preheader       string
	GroupedCode     string
	CreatedAt       string
	ExpiresAt       string
	OperatorName    string
	OperatorAddress string
	PrivacyURL      string
	LogoContentID   string
}) string {
	lines := []string{
		"Подтвердите регистрацию в «Ровном ходе»",
		"",
		"Введите этот код на странице регистрации оператора:",
		data.GroupedCode,
		"",
		"Запрос: " + data.CreatedAt + " МСК.",
		"Срок действия кода истекает: " + data.ExpiresAt + " МСК.",
		"",
		"Если вы не запрашивали регистрацию, просто проигнорируйте это письмо. Никому не сообщайте код.",
		"Сотрудники «Ровного хода» никогда не попросят код.",
		"",
		"Письмо отправлено автоматически, отвечать на него не нужно.",
		"«Ровный ход» · " + data.OperatorName + " · " + data.OperatorAddress,
	}
	if data.PrivacyURL != "" {
		lines = append(lines, "Политика конфиденциальности: "+data.PrivacyURL)
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}

func validOperatorDetail(value string) bool {
	if strings.TrimSpace(value) == "" || len(value) > 512 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"placeholder", "dummy", "demo", "example", "заглуш", "[", "]"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	return true
}

func validPrivacyURL(value string) bool {
	if !validOperatorDetail(value) {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !validSMTPHost(parsed.Hostname()) {
		return false
	}
	return true
}

func validRegistrationTarget(value string) bool {
	if !domain.ValidEmail(value) || domain.NormalizeEmail(value) != value {
		return false
	}
	address, err := mail.ParseAddress(value)
	return err == nil && address.Address == value && address.Name == ""
}

func validRegistrationID(value string) bool {
	return registrationIDPattern.MatchString(value)
}

func formatRegistrationTime(value time.Time) string {
	return value.In(registrationMSK).Format("02.01.2006, 15:04")
}

func formatRegistrationDeadline(value time.Time) string {
	return value.In(registrationMSK).Format("02.01.2006, 15:04:05")
}

func loadRegistrationMSK() *time.Location {
	location, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		return moscowLocation()
	}
	return location
}
