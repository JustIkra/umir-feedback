package application

import (
	"context"
	"errors"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

var (
	ErrIdentityConflict   = errors.New("submission identity conflict")
	ErrStorageUnavailable = errors.New("feedback storage unavailable")
	ErrNotFound           = errors.New("submission not found")
	ErrBusy               = errors.New("feedback store is locked")
)

type Repository interface {
	Accept(context.Context, domain.Submission) (domain.Receipt, bool, error)
	Lookup(context.Context, domain.SubmissionID) (domain.Submission, bool, error)
	Claim(context.Context, domain.Channel, time.Time, domain.RetryPolicy) (domain.Delivery, bool, error)
	Finish(context.Context, domain.SubmissionID, domain.Channel, domain.Job) error
}

type Sender interface {
	Send(context.Context, domain.Delivery) domain.DeliveryResult
}

type Clock interface{ Now() time.Time }
type Waiter interface {
	Wait(context.Context, time.Duration) error
}
type Jitter interface {
	Delay(time.Duration) time.Duration
}
type Normalizer interface{ NFC(string) string }

type ScopedRepository interface {
	LookupScoped(context.Context, domain.Namespace, domain.SubmissionID) (domain.Submission, bool, error)
	FinishScoped(context.Context, domain.Namespace, domain.SubmissionID, domain.Channel, domain.Job) error
}

var (
	ErrInvalidToken    = errors.New("invalid token")
	ErrAuthUnavailable = errors.New("auth unavailable")
	ErrForbidden       = errors.New("forbidden")
	ErrInvalidArgument = errors.New("invalid argument")
)
