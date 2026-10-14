package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

func historyObservationFixture(t *testing.T) (Config, domain.Submission, legacyEmailChallenge) {
	t.Helper()
	cfg := testConfig(t)
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.FixedZone("Europe/Moscow", 3*3600))
	cfg.Now = func() time.Time { return at.Add(time.Minute) }
	sub := testSubmission("123e4567-e89b-42d3-a456-426614174000", at)
	sub.Email = "history@umir.invalid"
	wire := toWireSubmission(sub)
	wire.Namespace, wire.CommandID, wire.Legacy = nil, "", false
	header, err := encodeRecord(record{Version: formatVersion, Type: "header"})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := encodeRecord(record{Version: formatVersion, Type: "accept", Submission: wire, Jobs: []wireJob{{Channel: "smtp", State: "pending", NextAttemptAt: at}, {Channel: "telegram", State: "pending", NextAttemptAt: at}}})
	if err != nil {
		t.Fatal(err)
	}
	archive := legacyEmailChallenge{ID: "11111111-1111-4111-8111-111111111111", RegistrationID: "22222222-2222-4222-8222-222222222222", Email: "archived@umir.invalid", Purpose: legacyOperatorRegistration, CreatedAt: at, ExpiresAt: at.Add(10 * time.Minute), ResendAvailableAt: at.Add(time.Minute), RetainUntil: at.Add(24 * time.Hour), State: legacyChallengePending, CodeDigest: bytes.Repeat([]byte{1}, 32), EncryptedCode: legacySealedValue{Nonce: bytes.Repeat([]byte{2}, 12), Ciphertext: bytes.Repeat([]byte{3}, 22)}, Issues: []legacyCodeIssuance{{ID: "11111111-1111-4111-8111-111111111111", AcceptedAt: at, ResendAvailableAt: at.Add(time.Minute), Job: domain.Job{Channel: domain.SMTP, State: domain.Pending, NextAttemptAt: at}}}}
	line, err := encodeChallengeRecord(archive)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := encodeRecord(record{Version: formatVersion, Type: "state", SubmissionID: string(sub.ID), Job: &wireJob{Channel: "smtp", State: "in-flight", Attempts: 1, StartedAt: at}})
	if err != nil {
		t.Fatal(err)
	}
	finish, err := encodeRecord(record{Version: formatVersion, Type: "state", SubmissionID: string(sub.ID), Job: &wireJob{Channel: "smtp", State: "sent", Attempts: 1, StartedAt: at, ConfirmedAt: at, ProviderID: "history-provider-sentinel"}})
	if err != nil {
		t.Fatal(err)
	}
	data := append(append(append(append(header, accepted...), claim...), finish...), line...)
	if err := os.WriteFile(cfg.Path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return cfg, sub, archive
}

func TestHistoryObservationReadsRealLegacyRecoveryAndPreservesReopen(t *testing.T) {
	cfg, sub, archive := historyObservationFixture(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if store != nil {
			if err := store.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	before, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	selectors := []Selector{{Namespace: domain.Anonymous(), ID: sub.ID}}
	ids := []ArchiveChallengeID{ArchiveChallengeID(archive.ID)}
	key := [32]byte{1}
	observed, err := store.ObserveHistory(context.Background(), selectors, ids, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(observed.Records) != 1 || len(observed.Archives) != 1 || observed.JournalBytes != int64(len(before)) {
		t.Fatal("incomplete real store projection")
	}
	row := observed.Records[0]
	if row.Namespace != domain.Anonymous() || row.Fingerprint != sub.Fingerprint || !row.AcceptedAt.Equal(sub.AcceptedAt) || !row.ReceivedAt.Equal(sub.ReceivedAt) || len(row.Jobs) != 2 || row.Jobs[0].JobID == "" || row.Jobs[1].JobID == "" || row.Jobs[0].State != domain.Sent || row.Jobs[0].Attempts != 1 || !row.Jobs[1].NextAttemptAt.Equal(sub.ReceivedAt) || len(row.Jobs[0].ProviderIDProof) != 64 {
		t.Fatal("original record, actual allocated job or deadline lost")
	}
	old := observed.Archives[0]
	if old.ChallengeID != ids[0] || !old.CreatedAt.Equal(archive.CreatedAt) || !old.ExpiresAt.Equal(archive.ExpiresAt) || !old.ResendAvailableAt.Equal(archive.ResendAvailableAt) || !old.RetainUntil.Equal(archive.RetainUntil) || len(old.Issues) != 1 || old.Issues[0].IssueID != string(archive.Issues[0].ID) || !old.Issues[0].AcceptedAt.Equal(archive.Issues[0].AcceptedAt) || !old.Issues[0].ResendAvailableAt.Equal(archive.Issues[0].ResendAvailableAt) || old.Issues[0].OriginalJob.JobID != "" {
		t.Fatal("archived original edges were changed or allocated new jobs")
	}
	raw, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"history@umir.invalid", "archived@umir.invalid", "history-provider-sentinel", sub.Name, sub.Phone, sub.Message, "CodeDigest", "EncryptedCode", "ProofDigest", "Ciphertext", "Nonce"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("private value or field exposed: %s", private)
		}
	}
	observed.Records[0].Jobs[0].Attempts = 100
	observed.Archives[0].Issues[0].IssueID = "changed"
	original, err := store.ObserveHistory(context.Background(), selectors, ids, key)
	if err != nil || original.Records[0].Jobs[0].Attempts != 1 || original.Archives[0].Issues[0].IssueID != string(archive.Issues[0].ID) {
		t.Fatal("caller mutated owner state through projection")
	}
	after, err := os.ReadFile(cfg.Path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read-only observation wrote journal bytes")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = nil
	cfg.Now = func() time.Time { return archive.RetainUntil.Add(time.Hour) }
	store, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := store.ObserveHistory(context.Background(), selectors, ids, key)
	if err != nil || !reflect.DeepEqual(original, reopened) {
		t.Fatal("real reopen changed retained identity, proofs or archive")
	}
}

func TestHistoryObservationRejectsExistingForeignAndFreshAnonymousRecords(t *testing.T) {
	cfg, legacy, archive := historyObservationFixture(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	fresh := testSubmission("33333333-3333-4333-8333-333333333333", legacy.ReceivedAt)
	fresh.Namespace = domain.Anonymous()
	fresh.CommandID = string(fresh.ID)
	scoped := fresh
	scoped.ID = "44444444-4444-4444-8444-444444444444"
	scoped.CommandID = string(scoped.ID)
	scoped.Namespace = domain.Namespace{Kind: "app", UserID: "55555555-5555-4555-8555-555555555555", OrgID: "66666666-6666-4666-8666-666666666666", TenantID: "77777777-7777-4777-8777-777777777777"}
	for _, value := range []domain.Submission{fresh, scoped} {
		if _, _, err := store.Accept(context.Background(), value); err != nil {
			t.Fatal(err)
		}
	}
	key := [32]byte{1}
	for _, selector := range []Selector{{Namespace: fresh.Namespace, ID: fresh.ID}, {Namespace: scoped.Namespace, ID: scoped.ID}, {Namespace: domain.Anonymous(), ID: "99999999-9999-4999-8999-999999999999"}} {
		if _, err := store.ObserveHistory(context.Background(), []Selector{selector}, nil, key); err == nil {
			t.Fatal("existing foreign/fresh or unknown record entered legacy projection")
		}
	}
	selector := Selector{Namespace: domain.Anonymous(), ID: legacy.ID}
	for _, selectors := range [][]Selector{{selector, selector}, make([]Selector, 17)} {
		if _, err := store.ObserveHistory(context.Background(), selectors, nil, key); err == nil {
			t.Fatal("duplicate or unbounded selectors admitted")
		}
	}
	if _, err := store.ObserveHistory(context.Background(), []Selector{selector}, []ArchiveChallengeID{ArchiveChallengeID(archive.ID), ArchiveChallengeID(archive.ID)}, key); err == nil {
		t.Fatal("duplicate archive admitted")
	}
	if _, err := store.ObserveHistory(context.Background(), []Selector{selector}, nil, [32]byte{}); err == nil {
		t.Fatal("empty observation proof key admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.ObserveHistory(ctx, []Selector{selector}, nil, key); err == nil {
		t.Fatal("cancelled observation admitted")
	}
}
