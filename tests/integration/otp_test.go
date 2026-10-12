package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/adapters/delivery"
	"github.com/JustIkra/umir-feedback/internal/adapters/httpapi"
	"github.com/JustIkra/umir-feedback/internal/adapters/journal"
	mechanisms "github.com/JustIkra/umir-feedback/internal/adapters/runtime"
	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

type otpClock struct{ now time.Time }

func (c *otpClock) Now() time.Time { return c.now }

type otpMailbox struct {
	received domain.CodeMail
	template *delivery.RegistrationTemplate
	err      error
}

func (m *otpMailbox) SendOTP(_ context.Context, mail domain.CodeMail) domain.DeliveryResult {
	m.received = mail
	_, m.err = m.template.Render(mail)
	return domain.DeliveryResult{Success: m.err == nil, Code: "test_accepted"}
}

func TestPrivateOTPProtocolRecoversProofAndScrubsCiphertext(t *testing.T) {
	ctx := context.Background()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	clock := &otpClock{now: time.Date(2027, time.January, 1, 12, 0, 0, 0, time.FixedZone("MSK", 10800))}
	config := journal.Config{Path: filepath.Join(dir, "journal.jsonl"), MaxBytes: 64 << 20, UID: os.Getuid(), GID: os.Getgid(), Policy: domain.DefaultRetryPolicy(), Now: clock.Now}
	store, err := journal.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	secrets, err := mechanisms.NewChallengeCrypto(bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	core, err := application.NewChallenges(ctx, store, secrets, clock, 4096)
	if err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler, err := httpapi.NewChallengeHandler(logger, core, token)
	if err != nil {
		t.Fatal(err)
	}
	post := func(path string, body map[string]string, want int) []byte {
		t.Helper()
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("status %d wanted %d", w.Code, want)
		}
		return w.Body.Bytes()
	}
	registration := "123e4567-e89b-42d3-a456-426614174000"
	base := map[string]string{"registrationID": registration, "purpose": "operator-registration"}
	begin := map[string]string{"registrationID": registration, "purpose": "operator-registration", "email": "Target@EXAMPLE.org"}
	first := post("/internal/v1/email-challenges", begin, 202)
	if !bytes.Equal(first, post("/internal/v1/email-challenges", begin, 202)) {
		t.Fatal("begin replay changed")
	}
	var receipt domain.ChallengeReceipt
	if err := json.Unmarshal(first, &receipt); err != nil {
		t.Fatal(err)
	}
	template, err := delivery.NewRegistrationTemplate(delivery.RegistrationMailConfig{OperatorName: "Тестовая организация", OperatorAddress: "Тестовый адрес, дом 1", SenderName: "Ровный ход"})
	if err != nil {
		t.Fatal(err)
	}
	mailbox := &otpMailbox{template: template}
	worked, err := core.ProcessOne(ctx, mailbox)
	if err != nil || !worked || mailbox.err != nil || mailbox.received.Email != "Target@example.org" {
		t.Fatal("queued mail not delivered to challenge target")
	}
	challenges, err := store.Challenges(ctx)
	if err != nil || len(challenges) != 1 {
		t.Fatal("missing challenge")
	}
	cipherBefore := base64.StdEncoding.EncodeToString(challenges[0].EncryptedCode.Ciphertext)
	verify := map[string]string{"registrationID": registration, "purpose": "operator-registration", "verificationID": "123e4567-e89b-42d3-a456-426614174001", "code": mailbox.received.Code}
	path := "/internal/v1/email-challenges/" + string(receipt.ChallengeID)
	verified := post(path+"/verify", verify, 200)
	var proof domain.ChallengeProof
	if err := json.Unmarshal(verified, &proof); err != nil || proof.ProofID == "" {
		t.Fatal("missing proof")
	}
	raw, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(cipherBefore)) || bytes.Contains(raw, []byte(proof.ProofID)) {
		t.Fatal("scrubbed code or plaintext proof retained")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = journal.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	core, err = application.NewChallenges(ctx, store, secrets, clock, 4096)
	if err != nil {
		t.Fatal(err)
	}
	handler, err = httpapi.NewChallengeHandler(logger, core, token)
	if err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(11 * time.Minute)
	if !bytes.Equal(verified, post(path+"/verify", verify, 200)) {
		t.Fatal("proof not recovered after restart and OTP expiry")
	}
	base["completionID"] = "123e4567-e89b-42d3-a456-426614174002"
	base["proofID"] = proof.ProofID
	completed := post(path+"/complete", base, 200)
	if !bytes.Equal(completed, post(path+"/complete", base, 200)) {
		t.Fatal("complete replay changed")
	}
	base["proofID"] = "123e4567-e89b-42d3-a456-426614174003"
	post(path+"/complete", base, 409)
	clock.now = proof.VerifiedAt.Add(24 * time.Hour)
	if err := core.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	post(path+"/verify", verify, 404)
	challenges, err = store.Challenges(ctx)
	if err != nil || len(challenges) != 0 {
		t.Fatal("recovery record outlived its window")
	}
	raw, err = os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Target@example.org") {
		t.Fatal("retained target survived tombstone deletion")
	}
}
