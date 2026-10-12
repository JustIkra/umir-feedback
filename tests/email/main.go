package main

import (
	"encoding/base64"
	"log"
	"os"
	"strings"
	"time"

	"github.com/JustIkra/umir-feedback/internal/adapters/delivery"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

func main() {
	template, err := delivery.NewRegistrationTemplate(delivery.RegistrationMailConfig{
		OperatorName:    "ООО «Проверка макета»",
		OperatorAddress: "г. Москва, ул. Примерная, д. 1",
		SenderName:      "Ровный ход",
	})
	if err != nil {
		log.Fatal(err)
	}
	location, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		log.Fatal(err)
	}
	createdAt := time.Date(2026, 10, 12, 15, 2, 0, 0, location)
	sendAt := createdAt.Add(3 * time.Minute)
	expiresAt := createdAt.Add(10 * time.Minute)
	if !sendAt.Before(expiresAt) {
		log.Fatal("preview resend time must precede the original expiry")
	}
	rendered, err := template.Render(domain.CodeMail{
		ChallengeID: "preview-challenge", IssuanceID: "preview-issuance", Email: "operator@example.test", Code: "012034",
		CreatedAt: createdAt, ExpiresAt: expiresAt,
	})
	if err != nil {
		log.Fatal(err)
	}
	preview := strings.Replace(rendered.HTML, "cid:"+rendered.LogoContentID, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(rendered.LogoPNG), 1)
	if err := os.WriteFile("tests/email/registration-preview.html", []byte(preview), 0o600); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("tests/email/registration-preview.txt", []byte(rendered.Text), 0o600); err != nil {
		log.Fatal(err)
	}

	oversizeTemplate, err := delivery.NewRegistrationTemplate(delivery.RegistrationMailConfig{
		OperatorName:    "ООО «Северный региональный оператор долгосрочного обслуживания»",
		OperatorAddress: "г. Москва, ул. " + strings.Repeat("Северная", 22) + ", дом 12",
		SenderName:      "Ровный ход",
	})
	if err != nil {
		log.Fatal(err)
	}
	oversizeRendered, err := oversizeTemplate.Render(domain.CodeMail{
		ChallengeID: "preview-oversize", IssuanceID: "preview-oversize-resend", Email: "operator@example.test", Code: "012034",
		CreatedAt: createdAt, ExpiresAt: expiresAt,
	})
	if err != nil {
		log.Fatal(err)
	}
	oversizeHTML := strings.Replace(oversizeRendered.HTML, "cid:"+oversizeRendered.LogoContentID, "registration-logo-disabled.png", 1)
	if err := os.WriteFile("tests/email/registration-preview-oversize-no-images.html", []byte(oversizeHTML), 0o600); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("tests/email/registration-preview-oversize-no-images.txt", []byte(oversizeRendered.Text), 0o600); err != nil {
		log.Fatal(err)
	}
}
