package bootstrap

import (
	"bytes"
	"context"
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

func TestRuntimeAcceptsWhileBothProvidersUnavailableAndJoins(t *testing.T) {
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
	closedAddress, ok := closed.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("listener did not have a TCP address")
	}
	port := closedAddress.Port
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	config := mechanisms.Config{StorePath: filepath.Join(dir, "journal.jsonl"), MaxStoreBytes: 1 << 20, RetentionDays: 30, ConsentVersion: "v1", Policy: domain.DefaultRetryPolicy(), PollInterval: time.Millisecond, SMTP: delivery.SMTPConfig{Host: "127.0.0.1", Port: port, TLSMode: "tls", Username: "dummy", Password: "dummy", From: "service@example.org", To: "operator@example.org"}, Telegram: delivery.TelegramConfig{Token: "123:dummy", ChatID: "-123", ProxyURL: "http://" + net.JoinHostPort("127.0.0.1", strings.Split(closed.Addr().String(), ":")[1]), ProxyUsername: "proxy", ProxyPassword: "dummy"}}
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, listener, slog.New(slog.NewTextHandler(&logs, nil)), config) }()
	client := &http.Client{Timeout: 3 * time.Second}
	payload := `{"submissionID":"123e4567-e89b-42d3-a456-426614174000","name":"private-name","email":"private@example.org","message":"private-message","consent":{"accepted":true,"documentVersion":"v1"},"website":""}`
	response, err := client.Post("http://"+listener.Addr().String()+"/feedback", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 201 {
		t.Fatalf("status %d %s", response.StatusCode, body)
	}
	var receipt domain.Receipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		t.Fatal(err)
	}
	_, offset := receipt.ReceivedAt.Zone()
	if offset != 10800 {
		t.Fatal("receipt timezone")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runtime goroutines did not join")
	}
	if strings.Contains(logs.String(), "private-") || strings.Contains(logs.String(), "private@example.org") {
		t.Fatal("personal data in logs")
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
	jobs, err := store.Status(context.Background())
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs %d %v", len(jobs), err)
	}
	for _, d := range jobs {
		if d.Job.State != domain.Pending {
			t.Fatalf("unavailable provider state %#v", d.Job)
		}
	}
}
