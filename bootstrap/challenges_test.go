package bootstrap

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/adapters/delivery"
	"github.com/JustIkra/umir-feedback/internal/adapters/journal"
	mechanisms "github.com/JustIkra/umir-feedback/internal/adapters/runtime"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

func TestRuntimePrivateOTPQueueAndFeedbackRemainIndependent(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := closed.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("TCP address required")
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	secrets, err := mechanisms.NewChallengeCrypto(bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	config := mechanisms.Config{StorePath: filepath.Join(dir, "journal.jsonl"), MaxStoreBytes: 64 << 20, RetentionDays: 30, ConsentVersion: "v1", Policy: domain.DefaultRetryPolicy(), PollInterval: time.Millisecond, SMTP: delivery.SMTPConfig{Host: "127.0.0.1", Port: addr.Port, TLSMode: "tls", Username: "dummy", Password: "dummy", From: "service@example.org", To: "operator@example.org"}, Telegram: delivery.TelegramConfig{Token: "123:dummy", ChatID: "-123", ProxyURL: "http://" + addr.String(), ProxyUsername: "proxy", ProxyPassword: "dummy"}, OTP: mechanisms.OTPConfig{Enabled: true, MaxChallenges: 4096, ServiceToken: token, Secrets: secrets, Mail: delivery.RegistrationMailConfig{OperatorName: "Тестовая организация", OperatorAddress: "Тестовый адрес, дом 1", SenderName: "Ровный ход"}}}
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, listener, slog.New(slog.NewTextHandler(&logs, nil)), config) }()
	client := &http.Client{Timeout: 3 * time.Second}
	base := "http://" + listener.Addr().String()
	post := func(path, body, auth string) (int, []byte) {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, data
	}
	begin := `{"registrationID":"123e4567-e89b-42d3-a456-426614174001","email":"target@example.org","purpose":"operator-registration"}`
	if status, _ := post("/internal/v1/email-challenges", begin, ""); status != 401 {
		t.Fatalf("private status %d", status)
	}
	status, data := post("/internal/v1/email-challenges", begin, token)
	if status != 202 {
		t.Fatalf("begin status %d", status)
	}
	var receipt domain.ChallengeReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.ChallengeID == "" {
		t.Fatal("missing challenge receipt")
	}
	if status, replayed := post("/internal/v1/email-challenges", begin, token); status != 202 || !bytes.Equal(data, replayed) {
		t.Fatal("begin replay differs")
	}
	feedback := `{"submissionID":"123e4567-e89b-42d3-a456-426614174002","name":"Private","email":"form@example.org","message":"Independent form","consent":{"accepted":true,"documentVersion":"v1"},"website":""}`
	if status, _ := post("/feedback", feedback, ""); status != 201 {
		t.Fatalf("feedback status %d", status)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OTP runtime did not join")
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), "target@example.org") {
		t.Fatal("secret or email in runtime logs")
	}
	store, err := journal.Open(journal.Config{Path: config.StorePath, MaxBytes: config.MaxStoreBytes, UID: os.Getuid(), GID: os.Getgid(), Policy: config.Policy})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	challenges, err := store.Challenges(context.Background())
	if err != nil || len(challenges) != 1 || len(challenges[0].Issues) != 1 {
		t.Fatal("challenge/job did not survive restart")
	}
	jobs, err := store.Status(context.Background())
	if err != nil || len(jobs) != 2 {
		t.Fatal("feedback jobs changed")
	}
}
