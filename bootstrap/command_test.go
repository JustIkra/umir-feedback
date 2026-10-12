package bootstrap

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/JustIkra/umir-feedback/internal/adapters/journal"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

func TestCommandStatusUsesJournalLock(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"FEEDBACK_STORE_PATH": filepath.Join(dir, "journal.jsonl"), "FEEDBACK_MAX_STORE_BYTES": "1048576", "FEEDBACK_RETENTION_DAYS": "30", "FEEDBACK_CONSENT_VERSION": "v1"}
	get := func(key string) string { return env[key] }
	store, err := journal.Open(journal.Config{Path: env["FEEDBACK_STORE_PATH"], MaxBytes: 1048576, UID: os.Getuid(), GID: os.Getgid(), Policy: domain.DefaultRetryPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Command(context.Background(), []string{"status"}, get, &out); err == nil {
		t.Fatal("command entered live journal")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Command(context.Background(), []string{"status"}, get, &out); err != nil {
		t.Fatal(err)
	}
}
func TestCommandRejectsUnknownArgumentsBeforeOpeningStore(t *testing.T) {
	var out bytes.Buffer
	err := Command(context.Background(), []string{"retry", "--channel", "mail"}, func(string) string { return "" }, &out)
	if err == nil {
		t.Fatal("invalid channel accepted")
	}
}
