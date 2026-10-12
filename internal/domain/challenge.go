package domain

import "time"

type ChallengeID string
type RegistrationID string
type IssuanceID string
type VerificationID string
type CompletionID string
type ChallengePurpose string
type ChallengeState string

const (
	OperatorRegistration ChallengePurpose = "operator-registration"
	ChallengePending     ChallengeState   = "pending"
	ChallengeVerified    ChallengeState   = "verified"
	ChallengeCompleted   ChallengeState   = "completed"
	ChallengeLocked      ChallengeState   = "locked"
	ChallengeExpired     ChallengeState   = "expired"
)

type SealedValue struct {
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

type CodeVerification struct {
	ID       VerificationID `json:"id"`
	Digest   []byte         `json:"digest"`
	Verified bool           `json:"verified"`
}

type CodeIssuance struct {
	ID                IssuanceID `json:"id"`
	AcceptedAt        time.Time  `json:"acceptedAt"`
	ResendAvailableAt time.Time  `json:"resendAvailableAt"`
	Job               Job        `json:"job"`
}

type EmailChallenge struct {
	ID                ChallengeID        `json:"id"`
	RegistrationID    RegistrationID     `json:"registrationID"`
	Email             string             `json:"email"`
	Purpose           ChallengePurpose   `json:"purpose"`
	CreatedAt         time.Time          `json:"createdAt"`
	ExpiresAt         time.Time          `json:"expiresAt"`
	ResendAvailableAt time.Time          `json:"resendAvailableAt"`
	RetainUntil       time.Time          `json:"retainUntil"`
	State             ChallengeState     `json:"state"`
	CodeDigest        []byte             `json:"codeDigest"`
	EncryptedCode     SealedValue        `json:"encryptedCode"`
	BadAttempts       int                `json:"badAttempts"`
	Verifications     []CodeVerification `json:"verifications"`
	Issues            []CodeIssuance     `json:"issues"`
	ProofDigest       []byte             `json:"proofDigest"`
	EncryptedProof    SealedValue        `json:"encryptedProof"`
	VerifiedAt        time.Time          `json:"verifiedAt"`
	CompletionID      CompletionID       `json:"completionID"`
	CompletedAt       time.Time          `json:"completedAt"`
}

type ChallengeReceipt struct {
	ChallengeID       ChallengeID `json:"challengeID"`
	ExpiresAt         time.Time   `json:"expiresAt"`
	ResendAvailableAt time.Time   `json:"resendAvailableAt"`
}

type ChallengeProof struct {
	ProofID    string    `json:"proofID"`
	VerifiedAt time.Time `json:"verifiedAt"`
}

type ChallengeCompletion struct {
	CompletedAt time.Time `json:"completedAt"`
}

type CodeMail struct {
	ChallengeID ChallengeID
	IssuanceID  IssuanceID
	Email       string
	Code        string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}
