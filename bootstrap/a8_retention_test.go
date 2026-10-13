package bootstrap

import (
	"bytes"
	"context"

	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/adapters/journal"
	mechanisms "github.com/JustIkra/umir-feedback/internal/adapters/runtime"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

func TestA8NoAutomaticPurgeAtStartup(t *testing.T) {
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	cfg := journal.Config{Path: filepath.Join(dir, "journal.jsonl"), MaxBytes: 16 << 20, UID: os.Getuid(), GID: os.Getgid(), Policy: domain.DefaultRetryPolicy()}
	s, e := journal.Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	loc, e := time.LoadLocation("Europe/Moscow")
	if e != nil {
		t.Fatal(e)
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, loc)
	sub := domain.Submission{ID: "123e4567-e89b-42d3-a456-426614174000", Name: "Synthetic", Email: "synthetic@umir.invalid", Message: "Synthetic", ConsentVersion: "v1", ConsentPurpose: "feedback-reply", AcceptedAt: old, ReceivedAt: old, Fingerprint: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	if _, _, e = s.Accept(context.Background(), sub); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	e = Run(context.Background(), ln, slog.New(slog.NewTextHandler(io.Discard, nil)), mechanisms.Config{StorePath: cfg.Path, MaxStoreBytes: cfg.MaxBytes, Policy: cfg.Policy, RetentionDays: 30})
	if e == nil {
		t.Fatal("invalid delivery setup accepted")
	}
	s, e = journal.Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if e := s.Close(); e != nil {
			t.Error(e)
		}
	}()
	_, found, e := s.Lookup(context.Background(), sub.ID)
	if e != nil || !found {
		t.Fatalf("runtime startup deleted retained intake after deadline: found=%v err=%v", found, e)
	}
}

func TestA8ExportRequiresAuthorizedGatewayScope(t *testing.T) {
	var out bytes.Buffer
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	env := map[string]string{"FEEDBACK_STORE_PATH": filepath.Join(dir, "journal.jsonl"), "FEEDBACK_MAX_STORE_BYTES": "1048576", "FEEDBACK_RETENTION_DAYS": "30", "FEEDBACK_CONSENT_VERSION": "v1"}
	err := Command(context.Background(), []string{"export"}, func(k string) string { return env[k] }, &out)
	if err == nil {
		t.Fatal("operator export opened local journal without authorized explicit restaurant/session")
	}
	if out.Len() != 0 {
		t.Fatal("unauthorized export emitted private data")
	}
}
