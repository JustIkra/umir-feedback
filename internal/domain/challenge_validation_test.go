package domain

import (
	"testing"
	"time"
)

func TestNormalizeChallengeEmail(t *testing.T) {
	got, err := NormalizeChallengeEmail("  Local+Tag@EXAMPLE.COM ")
	if err != nil || got != "Local+Tag@example.com" {
		t.Fatalf("NormalizeChallengeEmail() = %q, %v", got, err)
	}
	for _, email := range []string{"a@example.com, b@example.com", "Name <a@example.com>", "a@localhost", "a b@example.com", "a@example.com\\r\\nBcc:x@y.com"} {
		if _, err := NormalizeChallengeEmail(email); err == nil {
			t.Errorf("NormalizeChallengeEmail(%q) succeeded", email)
		}
	}
}

func TestCloneEmailChallengeDeepCopiesMutableValues(t *testing.T) {
	original := EmailChallenge{
		ID: "c", RegistrationID: "r", Email: "a@example.com", Purpose: OperatorRegistration,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute), RetainUntil: time.Now().Add(time.Hour), State: ChallengePending,
		CodeDigest: []byte{1}, EncryptedCode: SealedValue{Nonce: []byte{2}, Ciphertext: []byte{3}},
		Verifications: []CodeVerification{{ID: "v", Digest: []byte{4}}}, Issues: []CodeIssuance{{ID: "i", Job: Job{State: Pending}}},
		ProofDigest: []byte{5}, EncryptedProof: SealedValue{Nonce: []byte{6}, Ciphertext: []byte{7}},
	}
	copy := CloneEmailChallenge(original)
	copy.CodeDigest[0] = 10
	copy.EncryptedCode.Nonce[0] = 10
	copy.EncryptedCode.Ciphertext[0] = 10
	copy.Verifications[0].Digest[0] = 10
	copy.Issues[0].Job.State = Failed
	copy.ProofDigest[0] = 10
	copy.EncryptedProof.Ciphertext[0] = 10
	if original.CodeDigest[0] != 1 || original.EncryptedCode.Nonce[0] != 2 || original.EncryptedCode.Ciphertext[0] != 3 || original.Verifications[0].Digest[0] != 4 || original.Issues[0].Job.State != Pending || original.ProofDigest[0] != 5 || original.EncryptedProof.Ciphertext[0] != 7 {
		t.Fatal("clone shares mutable challenge data")
	}
}

func TestNormalizeChallengeEmailPreservesUnicodeLocalAndNormalizesDomain(t *testing.T) {
	got, err := NormalizeChallengeEmail(" Почта+Тег@ПРИМЕР.РФ ")
	if err != nil || got != "Почта+Тег@пример.рф" {
		t.Fatalf("NormalizeChallengeEmail() = %q, %v", got, err)
	}
}

func TestValidateEmailChallengeEnforcesSnapshotBoundsAndMoscowWindows(t *testing.T) {
	zone := time.FixedZone("Europe/Moscow", 3*60*60)
	created := time.Date(2026, 10, 12, 12, 0, 0, 0, zone)
	challenge := EmailChallenge{
		ID: "00000000-0000-4000-8000-000000000001", RegistrationID: "00000000-0000-4000-8000-000000000002",
		Email: "operator@example.com", Purpose: OperatorRegistration, CreatedAt: created,
		ExpiresAt: created.Add(10 * time.Minute), ResendAvailableAt: created.Add(time.Minute), RetainUntil: created.Add(24 * time.Hour),
		State: ChallengePending, CodeDigest: make([]byte, 32), EncryptedCode: SealedValue{Nonce: make([]byte, 12), Ciphertext: make([]byte, 22)},
		Issues: []CodeIssuance{{ID: "00000000-0000-4000-8000-000000000001", AcceptedAt: created, ResendAvailableAt: created.Add(time.Minute), Job: Job{Channel: SMTP, State: Pending}}},
	}
	if err := ValidateEmailChallenge(challenge); err != nil {
		t.Fatalf("valid challenge rejected: %v", err)
	}
	invalid := CloneEmailChallenge(challenge)
	invalid.EncryptedCode.Ciphertext = make([]byte, 23)
	if err := ValidateEmailChallenge(invalid); err == nil {
		t.Fatal("accepted ciphertext with invalid fixed size")
	}
	invalid = CloneEmailChallenge(challenge)
	invalid.ExpiresAt = invalid.ExpiresAt.Add(time.Nanosecond)
	if err := ValidateEmailChallenge(invalid); err == nil {
		t.Fatal("accepted noncanonical challenge TTL")
	}
	invalid = CloneEmailChallenge(challenge)
	invalid.CreatedAt = invalid.CreatedAt.UTC()
	if err := ValidateEmailChallenge(invalid); err == nil {
		t.Fatal("accepted UTC timestamp in persisted challenge")
	}
}
