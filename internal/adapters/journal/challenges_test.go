package journal

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

func testChallenge(now time.Time) domain.EmailChallenge {
	return domain.EmailChallenge{
		ID: "11111111-1111-4111-8111-111111111111", RegistrationID: "22222222-2222-4222-8222-222222222222",
		Email: "ada@example.test", Purpose: domain.OperatorRegistration, CreatedAt: now,
		ExpiresAt: now.Add(10 * time.Minute), ResendAvailableAt: now.Add(time.Minute), RetainUntil: now.Add(24 * time.Hour),
		State: domain.ChallengePending, CodeDigest: bytesOf(32, 0x31),
		EncryptedCode: domain.SealedValue{Nonce: bytesOf(12, 0x42), Ciphertext: bytesOf(22, 0x53)},
		Issues:        []domain.CodeIssuance{{ID: "11111111-1111-4111-8111-111111111111", AcceptedAt: now, ResendAvailableAt: now.Add(time.Minute), Job: domain.Job{Channel: domain.SMTP, State: domain.Pending, NextAttemptAt: now}}},
	}
}

func bytesOf(size int, value byte) []byte {
	result := make([]byte, size)
	for index := range result {
		result[index] = value
	}
	return result
}

func TestSaveChallengeAtomicallyPersistsChallengeAndInitialMailJob(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(moscowLocation())
	challenge := testChallenge(now)
	if err := store.SaveChallenge(context.Background(), challenge); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("journal has %d records, want header plus one atomic challenge/outbox record", len(lines))
	}
	if !strings.Contains(lines[1], `"type":"challenge"`) || !strings.Contains(lines[1], `"issues"`) || !strings.Contains(lines[1], `"job"`) {
		t.Fatalf("challenge record does not contain its initial outbox job: %s", lines[1])
	}

	store, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	got, err := store.Challenges(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != challenge.ID || got[0].Issues[0].ID != challenge.Issues[0].ID {
		t.Fatalf("Challenges() = %+v, %v; want persisted challenge and issue", got, err)
	}
}

func TestChallengesReturnsDeepCopy(t *testing.T) {
	store, err := Open(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, moscowLocation())
	challenge := testChallenge(now)
	if err := store.SaveChallenge(context.Background(), challenge); err != nil {
		t.Fatal(err)
	}
	got, err := store.Challenges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got[0].EncryptedCode.Ciphertext[0] ^= 0xff
	got[0].Issues[0].Job.State = domain.Failed
	again, err := store.Challenges(context.Background())
	if err != nil || again[0].EncryptedCode.Ciphertext[0] != challenge.EncryptedCode.Ciphertext[0] || again[0].Issues[0].Job.State != domain.Pending {
		t.Fatalf("returned challenge aliases journal state: %+v, %v", again, err)
	}
}

func TestPruneScrubsExpiredCodeAndRetainsBindingUntilWindowEnds(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, moscowLocation())
	challenge := testChallenge(now)
	if err := store.SaveChallenge(context.Background(), challenge); err != nil {
		t.Fatal(err)
	}
	if err := store.PruneChallenges(context.Background(), challenge.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	got, err := store.Challenges(context.Background())
	if err != nil || len(got) != 1 || len(got[0].EncryptedCode.Ciphertext) != 0 {
		t.Fatalf("expired challenge retention = %+v, %v; want binding retained and code cleared", got, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), base64.StdEncoding.EncodeToString(challenge.EncryptedCode.Ciphertext)) {
		t.Fatal("expired encrypted OTP remains in compacted journal bytes")
	}

	store, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := store.PruneChallenges(context.Background(), challenge.RetainUntil); err != nil {
		t.Fatal(err)
	}
	got, err = store.Challenges(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("challenge count after retention window = %d, %v; want 0", len(got), err)
	}
}

func TestSaveChallengeCipherRemovalCompactsAndSurvivesRestart(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, moscowLocation())
	challenge := testChallenge(now)
	if err := store.SaveChallenge(context.Background(), challenge); err != nil {
		t.Fatal(err)
	}
	challenge.State = domain.ChallengeExpired
	challenge.EncryptedCode = domain.SealedValue{}
	challenge.Issues[0].Job.State = domain.Failed
	challenge.Issues[0].Job.NextAttemptAt = time.Time{}
	challenge.Issues[0].Job.ConfirmedAt = now.Add(10 * time.Minute)
	challenge.Issues[0].Job.ErrorCode = "otp_expired"
	if err := store.SaveChallenge(context.Background(), challenge); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), base64.StdEncoding.EncodeToString(testChallenge(now).EncryptedCode.Ciphertext)) {
		t.Fatal("removed ciphertext remains in the active journal")
	}
	if len(strings.Split(strings.TrimSpace(string(data)), "\n")) != 2 {
		t.Fatal("cipher removal did not compact old challenge snapshots")
	}

	store, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	got, err := store.Challenges(context.Background())
	if err != nil || len(got) != 1 || got[0].State != domain.ChallengeExpired || len(got[0].EncryptedCode.Ciphertext) != 0 {
		t.Fatalf("recovered challenge = %+v, %v; want expired scrubbed binding", got, err)
	}
}

func TestVerifiedChallengeProofAndCompletionSurviveCompactionAndRestart(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(moscowLocation())
	challenge := testChallenge(now)
	if err := store.SaveChallenge(context.Background(), challenge); err != nil {
		t.Fatal(err)
	}
	challenge.State = domain.ChallengeVerified
	challenge.EncryptedCode = domain.SealedValue{}
	challenge.VerifiedAt = now.Add(time.Minute)
	challenge.RetainUntil = challenge.VerifiedAt.Add(24 * time.Hour)
	challenge.Verifications = []domain.CodeVerification{{ID: "44444444-4444-4444-8444-444444444444", Digest: bytesOf(32, 0x64), Verified: true}}
	challenge.ProofDigest = bytesOf(32, 0x65)
	challenge.EncryptedProof = domain.SealedValue{Nonce: bytesOf(12, 0x66), Ciphertext: bytesOf(52, 0x67)}
	challenge.Issues[0].Job.State = domain.Failed
	challenge.Issues[0].Job.NextAttemptAt = time.Time{}
	challenge.Issues[0].Job.ConfirmedAt = challenge.VerifiedAt
	challenge.Issues[0].Job.ErrorCode = "otp_consumed"
	if err := store.SaveChallenge(context.Background(), challenge); err != nil {
		t.Fatal(err)
	}
	challenge.State = domain.ChallengeCompleted
	challenge.CompletionID = "55555555-5555-4555-8555-555555555555"
	challenge.CompletedAt = challenge.VerifiedAt.Add(time.Second)
	if err := store.SaveChallenge(context.Background(), challenge); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	got, err := store.Challenges(context.Background())
	if err != nil || len(got) != 1 || got[0].State != domain.ChallengeCompleted || got[0].CompletionID != challenge.CompletionID || string(got[0].ProofDigest) != string(challenge.ProofDigest) || got[0].Verifications[0].ID != challenge.Verifications[0].ID || len(got[0].EncryptedCode.Ciphertext) != 0 {
		t.Fatalf("completed challenge replay state = %+v, %v", got, err)
	}
}

func TestOpenCompactsHistoricalCipherAfterDurableScrubSnapshot(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(moscowLocation())
	challenge := testChallenge(now)
	if err := store.SaveChallenge(context.Background(), challenge); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	challenge.State = domain.ChallengeExpired
	challenge.EncryptedCode = domain.SealedValue{}
	challenge.Issues[0].Job.State = domain.Failed
	challenge.Issues[0].Job.NextAttemptAt = time.Time{}
	challenge.Issues[0].Job.ConfirmedAt = now.Add(time.Second)
	challenge.Issues[0].Job.ErrorCode = "otp_expired"
	line, err := encodeChallengeRecord(challenge)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(cfg.Path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(line); err != nil {
		t.Fatal(errors.Join(err, file.Close()))
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	data, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), base64.StdEncoding.EncodeToString(testChallenge(now).EncryptedCode.Ciphertext)) {
		t.Fatal("startup left pre-scrub challenge ciphertext in the journal")
	}
}

func TestTailRepairPreservesCompleteChallengeSnapshot(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(moscowLocation())
	challenge := testChallenge(now)
	if err := store.SaveChallenge(context.Background(), challenge); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	validSize := info.Size()
	file, err := os.OpenFile(cfg.Path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"version":1,"type":"challenge","challenge":{"id":"partial"}`); err != nil {
		t.Fatal(errors.Join(err, file.Close()))
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	info, err = os.Stat(cfg.Path)
	if err != nil || info.Size() != validSize {
		t.Fatalf("repaired file size = %v, %v; want %d", info, err, validSize)
	}
	got, err := store.Challenges(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != challenge.ID {
		t.Fatalf("challenge after tail repair = %+v, %v", got, err)
	}
}

func TestFeedbackDeletionAndPurgeKeepChallengeRetention(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	now := time.Now().In(moscowLocation())
	if _, _, err := store.Accept(context.Background(), testSubmission("44444444-4444-4444-8444-444444444444", now)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveChallenge(context.Background(), testChallenge(now)); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteEmail(context.Background(), "ada@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := store.Purge(context.Background(), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := store.Challenges(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("challenge count after feedback deletion and purge = %d, %v; want 1", len(got), err)
	}
}

func TestOpenRejectsMalformedCompleteChallengeSnapshot(t *testing.T) {
	cfg := testConfig(t)
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(cfg.Path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString(`{"version":1,"type":"challenge","challenge":{"id":"x","unexpected":true}}` + "\n")
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(cfg); err == nil {
		t.Fatal(errors.Join(errors.New("Open() accepted a malformed complete challenge snapshot"), reopened.Close()))
	}
}

func TestChallengeCapacityReservesFutureSnapshotEvents(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxBytes = 8 << 20
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, moscowLocation())
	for index := 0; index < 100; index++ {
		challenge := testChallenge(now.Add(time.Duration(index) * time.Second))
		challenge.ID = domain.ChallengeID(fmt.Sprintf("11111111-1111-4111-8111-%012d", index))
		challenge.RegistrationID = domain.RegistrationID(fmt.Sprintf("22222222-2222-4222-8222-%012d", index))
		challenge.Issues[0].ID = domain.IssuanceID(challenge.ID)
		if err := store.SaveChallenge(context.Background(), challenge); errors.Is(err, application.ErrChallengeCapacity) {
			if index == 0 {
				t.Fatal("capacity reserve rejected even one bounded challenge")
			}
			return
		} else if err != nil {
			t.Fatalf("SaveChallenge(%d) = %v", index, err)
		}
	}
	t.Fatal("challenge growth did not reserve future snapshot events")
}

func TestChallengeReserveDoesNotDependOnFeedbackRetryPolicy(t *testing.T) {
	now := time.Now().In(moscowLocation())
	challenge := testChallenge(now)
	challenges := map[domain.ChallengeID]domain.EmailChallenge{challenge.ID: challenge}
	entries := make(map[domain.SubmissionID]*entry)
	low := reservedForState(entries, challenges, domain.RetryPolicy{MaxAttempts: 1})
	high := reservedForState(entries, challenges, domain.RetryPolicy{MaxAttempts: 10})
	if low != high {
		t.Fatalf("OTP reserve with feedback attempts 1=%d, 10=%d; want equal", low, high)
	}
}

func TestOneMiBStoreCanDurablyCompleteThreeOTPDeliveryAttempts(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxBytes = 1 << 20
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(moscowLocation())
	challenge := testChallenge(now)
	if err := store.SaveChallenge(context.Background(), challenge); err != nil {
		t.Fatalf("first challenge and outbox append: %v", err)
	}
	job := challenge.Issues[0].Job
	for attempt := 1; attempt <= 3; attempt++ {
		started := now.Add(time.Duration(attempt) * time.Second)
		job.State = domain.InFlight
		job.Attempts = attempt
		job.StartedAt = started
		job.NextAttemptAt = time.Time{}
		job.ConfirmedAt = time.Time{}
		job.ErrorCode = ""
		job.ProviderID = ""
		challenge.Issues[0].Job = job
		if err := store.SaveChallenge(context.Background(), challenge); err != nil {
			t.Fatalf("persist attempt %d claim: %v", attempt, err)
		}
		if attempt == 3 {
			job.State = domain.Sent
			job.ConfirmedAt = started.Add(time.Second)
			job.ProviderID = "smtp-message-3"
			challenge.Issues[0].Job = job
			if err := store.SaveChallenge(context.Background(), challenge); err != nil {
				t.Fatalf("persist final send: %v", err)
			}
			continue
		}
		job.State = domain.Pending
		job.NextAttemptAt = started.Add(2 * time.Second)
		challenge.Issues[0].Job = job
		if err := store.SaveChallenge(context.Background(), challenge); err != nil {
			t.Fatalf("persist attempt %d retry: %v", attempt, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	got, err := store.Challenges(context.Background())
	if err != nil || len(got) != 1 || got[0].Issues[0].Job.State != domain.Sent || got[0].Issues[0].Job.Attempts != 3 {
		t.Fatalf("recovered challenge delivery = %+v, %v; want sent on attempt 3", got, err)
	}
}

func TestMaximumValidatedChallengeSnapshotFitsEightKiB(t *testing.T) {
	now := time.Date(9999, 12, 29, 23, 59, 59, 999999999, moscowLocation())
	challenge := testChallenge(now)
	challenge.Email = strings.Repeat("a", 247) + "@a.test"
	challenge.State = domain.ChallengeCompleted
	challenge.EncryptedCode = domain.SealedValue{}
	challenge.VerifiedAt = now.Add(time.Minute)
	challenge.RetainUntil = challenge.VerifiedAt.Add(24 * time.Hour)
	challenge.CompletionID = "55555555-5555-4555-8555-555555555555"
	challenge.CompletedAt = challenge.VerifiedAt.Add(time.Second)
	challenge.BadAttempts = 4
	challenge.Verifications = make([]domain.CodeVerification, 5)
	for index := range challenge.Verifications {
		challenge.Verifications[index] = domain.CodeVerification{
			ID:     domain.VerificationID(fmt.Sprintf("44444444-4444-4444-8444-%012d", index)),
			Digest: bytesOf(32, byte(index+1)), Verified: index == 4,
		}
	}
	challenge.ProofDigest = bytesOf(32, 0x65)
	challenge.EncryptedProof = domain.SealedValue{Nonce: bytesOf(12, 0x66), Ciphertext: bytesOf(52, 0x67)}
	challenge.Issues = make([]domain.CodeIssuance, 4)
	maxTime := time.Date(9999, 12, 31, 23, 59, 59, 999999999, moscowLocation())
	for index := range challenge.Issues {
		accepted := now.Add(time.Duration(index) * time.Minute)
		challenge.Issues[index] = domain.CodeIssuance{
			ID:         domain.IssuanceID(fmt.Sprintf("11111111-1111-4111-8111-%012d", index)),
			AcceptedAt: accepted, ResendAvailableAt: accepted.Add(time.Minute),
			Job: domain.Job{Channel: domain.SMTP, State: domain.Failed, Attempts: 3, NextAttemptAt: maxTime, RetryStartedAt: maxTime, StartedAt: maxTime, ConfirmedAt: maxTime, ErrorCode: strings.Repeat("e", maxErrorCodeSize), ProviderID: strings.Repeat("p", maxProviderSize)},
		}
	}
	challenge.Issues[0].ID = domain.IssuanceID(challenge.ID)
	challenge.ResendAvailableAt = challenge.Issues[3].ResendAvailableAt
	if err := validateChallengeSnapshot(challenge); err != nil {
		t.Fatalf("maximum challenge fixture is invalid: %v", err)
	}
	line, err := encodeChallengeRecord(challenge)
	if err != nil {
		t.Fatal(err)
	}
	if len(line) > 8<<10 {
		t.Fatalf("maximum validated challenge snapshot is %d bytes, want <= %d", len(line), 8<<10)
	}
}
