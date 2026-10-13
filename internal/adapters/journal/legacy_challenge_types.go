package journal

import (
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

type legacyChallengeID string
type legacyRegistrationID string
type legacyIssuanceID string
type legacyVerificationID string
type legacyCompletionID string
type legacyChallengePurpose string
type legacyChallengeState string

const (
	legacyOperatorRegistration legacyChallengePurpose = "operator-registration"
	legacyChallengePending     legacyChallengeState   = "pending"
	legacyChallengeVerified    legacyChallengeState   = "verified"
	legacyChallengeCompleted   legacyChallengeState   = "completed"
	legacyChallengeLocked      legacyChallengeState   = "locked"
	legacyChallengeExpired     legacyChallengeState   = "expired"
)

type legacySealedValue struct {
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

type legacyCodeVerification struct {
	ID       legacyVerificationID `json:"id"`
	Digest   []byte               `json:"digest"`
	Verified bool                 `json:"verified"`
}

type legacyCodeIssuance struct {
	ID                legacyIssuanceID `json:"id"`
	AcceptedAt        time.Time        `json:"acceptedAt"`
	ResendAvailableAt time.Time        `json:"resendAvailableAt"`
	Job               domain.Job       `json:"job"`
}

type legacyEmailChallenge struct {
	ID                legacyChallengeID        `json:"id"`
	RegistrationID    legacyRegistrationID     `json:"registrationID"`
	Email             string                   `json:"email"`
	Purpose           legacyChallengePurpose   `json:"purpose"`
	CreatedAt         time.Time                `json:"createdAt"`
	ExpiresAt         time.Time                `json:"expiresAt"`
	ResendAvailableAt time.Time                `json:"resendAvailableAt"`
	RetainUntil       time.Time                `json:"retainUntil"`
	State             legacyChallengeState     `json:"state"`
	CodeDigest        []byte                   `json:"codeDigest"`
	EncryptedCode     legacySealedValue        `json:"encryptedCode"`
	BadAttempts       int                      `json:"badAttempts"`
	Verifications     []legacyCodeVerification `json:"verifications"`
	Issues            []legacyCodeIssuance     `json:"issues"`
	ProofDigest       []byte                   `json:"proofDigest"`
	EncryptedProof    legacySealedValue        `json:"encryptedProof"`
	VerifiedAt        time.Time                `json:"verifiedAt"`
	CompletionID      legacyCompletionID       `json:"completionID"`
	CompletedAt       time.Time                `json:"completedAt"`
}
