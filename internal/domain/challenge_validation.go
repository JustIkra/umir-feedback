package domain

import (
	"errors"
	"strings"
	"time"
)

var ErrInvalidEmailChallenge = errors.New("invalid email challenge")

func NormalizeChallengeEmail(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) > 254 || !ValidEmail(value) {
		return "", ErrInvalidEmailChallenge
	}
	return NormalizeEmail(value), nil
}

func CloneEmailChallenge(challenge EmailChallenge) EmailChallenge {
	challenge.CodeDigest = cloneBytes(challenge.CodeDigest)
	challenge.EncryptedCode = cloneSealed(challenge.EncryptedCode)
	challenge.Verifications = append([]CodeVerification(nil), challenge.Verifications...)
	for i := range challenge.Verifications {
		challenge.Verifications[i].Digest = cloneBytes(challenge.Verifications[i].Digest)
	}
	challenge.Issues = append([]CodeIssuance(nil), challenge.Issues...)
	challenge.ProofDigest = cloneBytes(challenge.ProofDigest)
	challenge.EncryptedProof = cloneSealed(challenge.EncryptedProof)
	return challenge
}

func ValidateEmailChallenge(challenge EmailChallenge) error {
	if !validUUIDv4(string(challenge.ID)) || !validUUIDv4(string(challenge.RegistrationID)) || challenge.Purpose != OperatorRegistration {
		return ErrInvalidEmailChallenge
	}
	email, err := NormalizeChallengeEmail(challenge.Email)
	if err != nil || email != challenge.Email {
		return ErrInvalidEmailChallenge
	}
	if !validMoscowTime(challenge.CreatedAt) || !validMoscowTime(challenge.ExpiresAt) || !validMoscowTime(challenge.ResendAvailableAt) || !validMoscowTime(challenge.RetainUntil) || challenge.ExpiresAt.Sub(challenge.CreatedAt) != 10*time.Minute || len(challenge.CodeDigest) != 32 || len(challenge.Verifications) > 5 || len(challenge.Issues) == 0 || len(challenge.Issues) > 4 || challenge.BadAttempts < 0 || challenge.BadAttempts > 5 {
		return ErrInvalidEmailChallenge
	}
	if challenge.RetainUntil.Sub(challenge.CreatedAt) != 24*time.Hour && (challenge.VerifiedAt.IsZero() || challenge.RetainUntil.Sub(challenge.VerifiedAt) != 24*time.Hour) {
		return ErrInvalidEmailChallenge
	}
	switch challenge.State {
	case ChallengePending:
		if len(challenge.EncryptedCode.Nonce) != 12 || len(challenge.EncryptedCode.Ciphertext) != 22 || len(challenge.ProofDigest) != 0 || len(challenge.EncryptedProof.Nonce) != 0 || len(challenge.EncryptedProof.Ciphertext) != 0 || !challenge.VerifiedAt.IsZero() || challenge.CompletionID != "" || !challenge.CompletedAt.IsZero() || challenge.BadAttempts >= 5 {
			return ErrInvalidEmailChallenge
		}
	case ChallengeVerified, ChallengeCompleted:
		if len(challenge.EncryptedCode.Nonce) != 0 || len(challenge.EncryptedCode.Ciphertext) != 0 || !validMoscowTime(challenge.VerifiedAt) || challenge.VerifiedAt.Before(challenge.CreatedAt) || !challenge.VerifiedAt.Before(challenge.ExpiresAt) || len(challenge.ProofDigest) != 32 || len(challenge.EncryptedProof.Nonce) != 12 || len(challenge.EncryptedProof.Ciphertext) != 52 || challenge.RetainUntil.Sub(challenge.VerifiedAt) != 24*time.Hour {
			return ErrInvalidEmailChallenge
		}
		if challenge.State == ChallengeCompleted && (!validUUIDv4(string(challenge.CompletionID)) || !validMoscowTime(challenge.CompletedAt) || challenge.CompletedAt.Before(challenge.VerifiedAt) || !challenge.CompletedAt.Before(challenge.RetainUntil)) {
			return ErrInvalidEmailChallenge
		}
		if challenge.State == ChallengeVerified && (challenge.CompletionID != "" || !challenge.CompletedAt.IsZero()) {
			return ErrInvalidEmailChallenge
		}
	case ChallengeLocked:
		if challenge.BadAttempts != 5 || len(challenge.EncryptedCode.Nonce) != 0 || len(challenge.EncryptedCode.Ciphertext) != 0 || len(challenge.ProofDigest) != 0 || len(challenge.EncryptedProof.Nonce) != 0 || len(challenge.EncryptedProof.Ciphertext) != 0 || challenge.CompletionID != "" {
			return ErrInvalidEmailChallenge
		}
	case ChallengeExpired:
		if len(challenge.EncryptedCode.Nonce) != 0 || len(challenge.EncryptedCode.Ciphertext) != 0 || len(challenge.ProofDigest) != 0 || len(challenge.EncryptedProof.Nonce) != 0 || len(challenge.EncryptedProof.Ciphertext) != 0 || challenge.CompletionID != "" {
			return ErrInvalidEmailChallenge
		}
	default:
		return ErrInvalidEmailChallenge
	}
	badAttempts := 0
	verifiedAttempts := 0
	verificationIDs := make(map[VerificationID]struct{}, len(challenge.Verifications))
	for _, verification := range challenge.Verifications {
		if !validUUIDv4(string(verification.ID)) || len(verification.Digest) != 32 {
			return ErrInvalidEmailChallenge
		}
		if _, ok := verificationIDs[verification.ID]; ok {
			return ErrInvalidEmailChallenge
		}
		verificationIDs[verification.ID] = struct{}{}
		if !verification.Verified {
			badAttempts++
		} else {
			verifiedAttempts++
		}
	}
	if badAttempts != challenge.BadAttempts || (challenge.State == ChallengePending || challenge.State == ChallengeLocked || challenge.State == ChallengeExpired) && verifiedAttempts != 0 || (challenge.State == ChallengeVerified || challenge.State == ChallengeCompleted) && verifiedAttempts != 1 {
		return ErrInvalidEmailChallenge
	}
	issueIDs := make(map[IssuanceID]struct{}, len(challenge.Issues))
	for i, issue := range challenge.Issues {
		if !validUUIDv4(string(issue.ID)) || !validMoscowTime(issue.AcceptedAt) || !validMoscowTime(issue.ResendAvailableAt) || issue.ResendAvailableAt.Sub(issue.AcceptedAt) != 60*time.Second || !issue.AcceptedAt.Before(challenge.ExpiresAt) || issue.Job.Channel != SMTP || issue.Job.Attempts < 0 || issue.Job.Attempts > 3 {
			return ErrInvalidEmailChallenge
		}
		if i == 0 && (issue.ID != IssuanceID(challenge.ID) || !issue.AcceptedAt.Equal(challenge.CreatedAt)) {
			return ErrInvalidEmailChallenge
		}
		if i > 0 && issue.AcceptedAt.Before(challenge.Issues[i-1].AcceptedAt.Add(60*time.Second)) {
			return ErrInvalidEmailChallenge
		}
		if _, ok := issueIDs[issue.ID]; ok {
			return ErrInvalidEmailChallenge
		}
		issueIDs[issue.ID] = struct{}{}
		switch issue.Job.State {
		case Pending, InFlight, Sent, Failed:
		default:
			return ErrInvalidEmailChallenge
		}
		if (challenge.State == ChallengeVerified || challenge.State == ChallengeCompleted || challenge.State == ChallengeLocked || challenge.State == ChallengeExpired) && (issue.Job.State == Pending || issue.Job.State == InFlight) {
			return ErrInvalidEmailChallenge
		}
	}
	if !challenge.Issues[len(challenge.Issues)-1].ResendAvailableAt.Equal(challenge.ResendAvailableAt) {
		return ErrInvalidEmailChallenge
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

func cloneSealed(value SealedValue) SealedValue {
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
