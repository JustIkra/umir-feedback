package journal

import (
	"errors"
	"strings"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

var errLegacyInvalidEmailChallenge = errors.New("invalid email challenge")

func legacyNormalizeChallengeEmail(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) > 254 || !domain.ValidEmail(value) {
		return "", errLegacyInvalidEmailChallenge
	}
	return domain.NormalizeEmail(value), nil
}

func legacyCloneEmailChallenge(challenge legacyEmailChallenge) legacyEmailChallenge {
	challenge.CodeDigest = cloneBytes(challenge.CodeDigest)
	challenge.EncryptedCode = cloneSealed(challenge.EncryptedCode)
	challenge.Verifications = append([]legacyCodeVerification(nil), challenge.Verifications...)
	for i := range challenge.Verifications {
		challenge.Verifications[i].Digest = cloneBytes(challenge.Verifications[i].Digest)
	}
	challenge.Issues = append([]legacyCodeIssuance(nil), challenge.Issues...)
	challenge.ProofDigest = cloneBytes(challenge.ProofDigest)
	challenge.EncryptedProof = cloneSealed(challenge.EncryptedProof)
	return challenge
}

func legacyValidateEmailChallenge(challenge legacyEmailChallenge) error {
	if !validUUIDv4(string(challenge.ID)) || !validUUIDv4(string(challenge.RegistrationID)) || challenge.Purpose != legacyOperatorRegistration {
		return errLegacyInvalidEmailChallenge
	}
	email, err := legacyNormalizeChallengeEmail(challenge.Email)
	if err != nil || email != challenge.Email {
		return errLegacyInvalidEmailChallenge
	}
	if !validMoscowTime(challenge.CreatedAt) || !validMoscowTime(challenge.ExpiresAt) || !validMoscowTime(challenge.ResendAvailableAt) || !validMoscowTime(challenge.RetainUntil) || challenge.ExpiresAt.Sub(challenge.CreatedAt) != 10*time.Minute || len(challenge.CodeDigest) != 32 || len(challenge.Verifications) > 5 || len(challenge.Issues) == 0 || len(challenge.Issues) > 4 || challenge.BadAttempts < 0 || challenge.BadAttempts > 5 {
		return errLegacyInvalidEmailChallenge
	}
	if challenge.RetainUntil.Sub(challenge.CreatedAt) != 24*time.Hour && (challenge.VerifiedAt.IsZero() || challenge.RetainUntil.Sub(challenge.VerifiedAt) != 24*time.Hour) {
		return errLegacyInvalidEmailChallenge
	}
	switch challenge.State {
	case legacyChallengePending:
		if len(challenge.EncryptedCode.Nonce) != 12 || len(challenge.EncryptedCode.Ciphertext) != 22 || len(challenge.ProofDigest) != 0 || len(challenge.EncryptedProof.Nonce) != 0 || len(challenge.EncryptedProof.Ciphertext) != 0 || !challenge.VerifiedAt.IsZero() || challenge.CompletionID != "" || !challenge.CompletedAt.IsZero() || challenge.BadAttempts >= 5 {
			return errLegacyInvalidEmailChallenge
		}
	case legacyChallengeVerified, legacyChallengeCompleted:
		if len(challenge.EncryptedCode.Nonce) != 0 || len(challenge.EncryptedCode.Ciphertext) != 0 || !validMoscowTime(challenge.VerifiedAt) || challenge.VerifiedAt.Before(challenge.CreatedAt) || !challenge.VerifiedAt.Before(challenge.ExpiresAt) || len(challenge.ProofDigest) != 32 || len(challenge.EncryptedProof.Nonce) != 12 || len(challenge.EncryptedProof.Ciphertext) != 52 || challenge.RetainUntil.Sub(challenge.VerifiedAt) != 24*time.Hour {
			return errLegacyInvalidEmailChallenge
		}
		if challenge.State == legacyChallengeCompleted && (!validUUIDv4(string(challenge.CompletionID)) || !validMoscowTime(challenge.CompletedAt) || challenge.CompletedAt.Before(challenge.VerifiedAt) || !challenge.CompletedAt.Before(challenge.RetainUntil)) {
			return errLegacyInvalidEmailChallenge
		}
		if challenge.State == legacyChallengeVerified && (challenge.CompletionID != "" || !challenge.CompletedAt.IsZero()) {
			return errLegacyInvalidEmailChallenge
		}
	case legacyChallengeLocked:
		if challenge.BadAttempts != 5 || len(challenge.EncryptedCode.Nonce) != 0 || len(challenge.EncryptedCode.Ciphertext) != 0 || len(challenge.ProofDigest) != 0 || len(challenge.EncryptedProof.Nonce) != 0 || len(challenge.EncryptedProof.Ciphertext) != 0 || challenge.CompletionID != "" {
			return errLegacyInvalidEmailChallenge
		}
	case legacyChallengeExpired:
		if len(challenge.EncryptedCode.Nonce) != 0 || len(challenge.EncryptedCode.Ciphertext) != 0 || len(challenge.ProofDigest) != 0 || len(challenge.EncryptedProof.Nonce) != 0 || len(challenge.EncryptedProof.Ciphertext) != 0 || challenge.CompletionID != "" {
			return errLegacyInvalidEmailChallenge
		}
	default:
		return errLegacyInvalidEmailChallenge
	}
	badAttempts := 0
	verifiedAttempts := 0
	verificationIDs := make(map[legacyVerificationID]struct{}, len(challenge.Verifications))
	for _, verification := range challenge.Verifications {
		if !validUUIDv4(string(verification.ID)) || len(verification.Digest) != 32 {
			return errLegacyInvalidEmailChallenge
		}
		if _, ok := verificationIDs[verification.ID]; ok {
			return errLegacyInvalidEmailChallenge
		}
		verificationIDs[verification.ID] = struct{}{}
		if !verification.Verified {
			badAttempts++
		} else {
			verifiedAttempts++
		}
	}
	if badAttempts != challenge.BadAttempts || (challenge.State == legacyChallengePending || challenge.State == legacyChallengeLocked || challenge.State == legacyChallengeExpired) && verifiedAttempts != 0 || (challenge.State == legacyChallengeVerified || challenge.State == legacyChallengeCompleted) && verifiedAttempts != 1 {
		return errLegacyInvalidEmailChallenge
	}
	issueIDs := make(map[legacyIssuanceID]struct{}, len(challenge.Issues))
	for i, issue := range challenge.Issues {
		if !validUUIDv4(string(issue.ID)) || !validMoscowTime(issue.AcceptedAt) || !validMoscowTime(issue.ResendAvailableAt) || issue.ResendAvailableAt.Sub(issue.AcceptedAt) != 60*time.Second || !issue.AcceptedAt.Before(challenge.ExpiresAt) || issue.Job.Channel != domain.SMTP || issue.Job.Attempts < 0 || issue.Job.Attempts > 3 {
			return errLegacyInvalidEmailChallenge
		}
		if i == 0 && (issue.ID != legacyIssuanceID(challenge.ID) || !issue.AcceptedAt.Equal(challenge.CreatedAt)) {
			return errLegacyInvalidEmailChallenge
		}
		if i > 0 && issue.AcceptedAt.Before(challenge.Issues[i-1].AcceptedAt.Add(60*time.Second)) {
			return errLegacyInvalidEmailChallenge
		}
		if _, ok := issueIDs[issue.ID]; ok {
			return errLegacyInvalidEmailChallenge
		}
		issueIDs[issue.ID] = struct{}{}
		switch issue.Job.State {
		case domain.Pending, domain.InFlight, domain.Sent, domain.Failed:
		default:
			return errLegacyInvalidEmailChallenge
		}
		if (challenge.State == legacyChallengeVerified || challenge.State == legacyChallengeCompleted || challenge.State == legacyChallengeLocked || challenge.State == legacyChallengeExpired) && (issue.Job.State == domain.Pending || issue.Job.State == domain.InFlight) {
			return errLegacyInvalidEmailChallenge
		}
	}
	if !challenge.Issues[len(challenge.Issues)-1].ResendAvailableAt.Equal(challenge.ResendAvailableAt) {
		return errLegacyInvalidEmailChallenge
	}
	return nil
}

func validUUIDv4(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' || !strings.ContainsRune("89ab", rune(value[19])) {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validMoscowTime(value time.Time) bool {
	if value.IsZero() {
		return false
	}
	_, offset := value.Zone()
	return offset == 3*60*60
}

func cloneSealed(value legacySealedValue) legacySealedValue {
	value.Nonce = cloneBytes(value.Nonce)
	value.Ciphertext = cloneBytes(value.Ciphertext)
	return value
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}
