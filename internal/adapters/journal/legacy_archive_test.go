package journal

import (
	"bytes"
	"os"

	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

func TestArchivedOTPSnapshotIsReadOnlyAcrossOpenAndCompaction(t *testing.T) {
	cfg := testConfig(t)
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.FixedZone("Europe/Moscow", 3*3600))
	cfg.Now = func() time.Time { return now.Add(48 * time.Hour) }
	header, err := encodeRecord(record{Version: formatVersion, Type: "header"})
	if err != nil {
		t.Fatal(err)
	}
	c := legacyEmailChallenge{ID: "11111111-1111-4111-8111-111111111111", RegistrationID: "22222222-2222-4222-8222-222222222222", Email: "archived@umir.invalid", Purpose: legacyOperatorRegistration, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute), ResendAvailableAt: now.Add(time.Minute), RetainUntil: now.Add(24 * time.Hour), State: legacyChallengePending, CodeDigest: bytes.Repeat([]byte{1}, 32), EncryptedCode: legacySealedValue{Nonce: bytes.Repeat([]byte{2}, 12), Ciphertext: bytes.Repeat([]byte{3}, 22)}, Issues: []legacyCodeIssuance{{ID: "11111111-1111-4111-8111-111111111111", AcceptedAt: now, ResendAvailableAt: now.Add(time.Minute), Job: domain.Job{Channel: domain.SMTP, State: domain.Pending, NextAttemptAt: now}}}}
	line, err := encodeChallengeRecord(c)
	if err != nil {
		t.Fatal(err)
	}
	original := append(header, line...)
	if err := os.WriteFile(cfg.Path, original, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	b, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, original) {
		t.Fatal("open altered historical OTP evidence")
	}
	decoded, err := encodeChallengeRecord(s.challenges[c.ID])
	if err != nil || !bytes.Equal(decoded, line) {
		t.Fatal("legacy snapshot changed on decode")
	}
	if err := s.compact(s.entries, s.challenges); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, original) {
		t.Fatal("Feedback compaction dropped or rewrote archived snapshot")
	}
}
