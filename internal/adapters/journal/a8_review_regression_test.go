package journal

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

func a8LegacySnapshotBaseline(t *testing.T) (Config, domain.Submission) {
	t.Helper()
	cfg := testConfig(t)
	at := time.Date(2026, 10, 13, 9, 0, 0, 0, domain.MoscowLocation())
	cfg.Now = func() time.Time { return at }
	sub := testSubmission("123e4567-e89b-42d3-a456-426614174000", at)
	wire := toWireSubmission(sub)
	wire.Namespace = nil
	wire.CommandID = ""
	wire.Legacy = false
	header, err := encodeRecord(record{Version: 1, Type: "header"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := encodeRecord(record{Version: 1, Type: "snapshot", Submission: wire, Jobs: []wireJob{{Channel: "smtp", State: "pending", NextAttemptAt: at}, {Channel: "telegram", State: "pending", NextAttemptAt: at}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cfg.Path, append(header, snapshot...), 0600); err != nil {
		t.Fatal(err)
	}
	return cfg, sub
}

func TestA8ReviewLegacySnapshotHasDurableJobs(t *testing.T) {
	cfg, sub := a8LegacySnapshotBaseline(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	rows, complete, err := store.Observe(context.Background(), []Selector{{domain.Anonymous(), sub.ID}})
	if err != nil || !complete {
		t.Fatal(err)
	}
	if rows[0].Jobs[0].JobID == "" || rows[0].Jobs[1].JobID == "" || rows[0].Jobs[0].JobID == rows[0].Jobs[1].JobID {
		t.Fatalf("version 1 snapshot reopened READY with invalid jobs: %+v", rows[0].Jobs)
	}
}

func TestA8ReviewLegacySnapshotTransitionSurvivesReopen(t *testing.T) {
	cfg, _ := a8LegacySnapshotBaseline(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, found, err := store.Claim(context.Background(), domain.SMTP, cfg.Now(), cfg.Policy)
	if err != nil || !found {
		t.Fatal("claim", found, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(cfg)
	if err != nil {
		t.Fatalf("claim on a supported legacy snapshot makes later reopen fail: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestA8ReviewSnapshotRecoveryRejectsCrossNamespaceAlias(t *testing.T) {
	cfg := testConfig(t)
	at := time.Date(2026, 10, 13, 9, 0, 0, 0, domain.MoscowLocation())
	cfg.Now = func() time.Time { return at }
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	one := testSubmission("123e4567-e89b-42d3-a456-426614174000", at)
	one.Namespace = domain.Namespace{Kind: "app", UserID: "11111111-1111-4111-8111-111111111111", OrgID: "22222222-2222-4222-8222-222222222222", TenantID: "33333333-3333-4333-8333-333333333333"}
	two := one
	two.Namespace.TenantID = "44444444-4444-4444-8444-444444444444"
	for _, sub := range []domain.Submission{one, two} {
		if _, _, err := store.Accept(context.Background(), sub); err != nil {
			t.Fatal(err)
		}
	}
	rows, _, err := store.Observe(context.Background(), []Selector{{one.Namespace, one.ID}, {two.Namespace, two.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.compactForTest(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.ReplaceAll(data, []byte(rows[1].Jobs[0].JobID), []byte(rows[0].Jobs[0].JobID))
	if err := os.WriteFile(cfg.Path, data, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(cfg)
	if err == nil {
		rows, complete, observeErr := reopened.Observe(context.Background(), []Selector{{one.Namespace, one.ID}, {two.Namespace, two.ID}})
		if closeErr := reopened.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		t.Fatalf("snapshot alias accepted as READY; complete=%v err=%v jobs=%+v", complete, observeErr, rows)
	}
}
