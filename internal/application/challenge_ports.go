package application

import (
	"context"
	"errors"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

var (
	ErrChallengeUnavailable         = errors.New("challenge unavailable")
	ErrChallengeIdentityConflict    = errors.New("challenge identity conflict")
	ErrVerificationIdentityConflict = errors.New("verification identity conflict")
	ErrCompletionIdentityConflict   = errors.New("completion identity conflict")
	ErrInvalidCode                  = errors.New("invalid code")
	ErrInvalidChallengeRequest      = errors.New("invalid challenge request")
	ErrStorageOutcomeUnknown        = errors.New("feedback outcome unknown")
	ErrChallengeCapacity            = errors.New("challenge capacity exhausted")
)

type ChallengeRateLimited struct{ RetryAfter time.Duration }

func (e ChallengeRateLimited) Error() string { return "challenge rate limited" }

type ChallengeRepository interface {
	Challenges(context.Context) ([]domain.EmailChallenge, error)
	SaveChallenge(context.Context, domain.EmailChallenge) error
	PruneChallenges(context.Context, time.Time) error
}

type ChallengeSecrets interface {
	NewID() (string, error)
	NewCode() (string, error)
	Digest(string, string) []byte
	Equal([]byte, []byte) bool
	Seal(string, string) (domain.SealedValue, error)
	Open(string, domain.SealedValue) (string, error)
}

type CodeSender interface {
	SendOTP(context.Context, domain.CodeMail) domain.DeliveryResult
}

type ChallengeBegin struct {
	RegistrationID domain.RegistrationID
	Email          string
	Purpose        domain.ChallengePurpose
}
type ChallengeResend struct {
	ChallengeID    domain.ChallengeID
	RegistrationID domain.RegistrationID
	Purpose        domain.ChallengePurpose
	IssuanceID     domain.IssuanceID
}
type ChallengeVerify struct {
	ChallengeID    domain.ChallengeID
	RegistrationID domain.RegistrationID
	Purpose        domain.ChallengePurpose
	VerificationID domain.VerificationID
	Code           string
}
type ChallengeComplete struct {
	ChallengeID    domain.ChallengeID
	RegistrationID domain.RegistrationID
	Purpose        domain.ChallengePurpose
	CompletionID   domain.CompletionID
	ProofID        string
}
