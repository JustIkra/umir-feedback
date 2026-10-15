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
	// ErrLeaseLost means the delivery lease expired and another claim took the job over.
	ErrLeaseLost = errors.New("delivery lease lost")
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

// DeliveryOutcome is the closed set of delivery transitions an observer sees.
type DeliveryOutcome string

const (
	DeliverySent    DeliveryOutcome = "sent"
	DeliveryRetry   DeliveryOutcome = "retry"
	DeliveryFailed  DeliveryOutcome = "failed"
	DeliveryUnknown DeliveryOutcome = "unknown"
)

// DeliveryCause is the closed set of reasons a result could not be stored.
type DeliveryCause string

const (
	CauseStoreError DeliveryCause = "store_error"
	CauseLeaseLost  DeliveryCause = "lease_lost"
	CauseNotFound   DeliveryCause = "not_found"
)

// DeliveryEvent describes one delivery attempt result. It carries no personal
// data: Code is a value from the closed delivery result code set, never provider text.
type DeliveryEvent struct {
	Outcome DeliveryOutcome
	Channel domain.Channel
	// Code is the result code of the attempt (for example smtp_sent), also set for sent and unknown.
	Code string
	// Cause explains an unknown outcome: one of the DeliveryCause constants, empty otherwise.
	Cause        DeliveryCause
	Attempt      int
	JobID        string
	SubmissionID domain.SubmissionID
}

// DeliveryObserver receives one event per delivery transition.
// DeliveryUnknown means the result could not be confirmed in storage.
type DeliveryObserver interface{ ObserveDelivery(DeliveryEvent) }
