package application

import (
	"context"
	"errors"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

var (
	// ErrQuotaExceeded means the live-row quota of the namespace is full; the request is stored nowhere.
	ErrQuotaExceeded = errors.New("feedback quota exceeded")
	// ErrScopeDeleted means the restaurant or organization of the request was deleted in Auth.
	ErrScopeDeleted = errors.New("feedback scope deleted")
	// ErrNotRetryable means an administrative retry does not apply to the delivery.
	ErrNotRetryable = errors.New("delivery cannot be retried")
)

// Fingerprinter turns the binding and normalized content of a submission into the
// stored fingerprint: 64 lower-case hex characters. Production uses a keyed HMAC.
type Fingerprinter interface {
	Sum(binding string, content []byte) string
}

// RetentionConfig holds the retention periods in days (R9, G1-2).
type RetentionConfig struct {
	AnonymousDays    int
	RestaurantDays   int
	DeletedScopeDays int
	TombstoneDays    int
}

// QuotaConfig limits the live (not erased) rows per namespace.
type QuotaConfig struct {
	Anonymous  int
	Restaurant int
}

// ListPosition is the keyset position in a restaurant's submission list; the zero value starts at the beginning.
type ListPosition struct {
	ReceivedAt time.Time
	Seq        int64
}

// ListedSubmission is one row of a scoped list with its position.
type ListedSubmission struct {
	Submission domain.Submission
	Position   ListPosition
}

// ScopedLister lists the live submissions of one restaurant in keyset order (received time, then sequence).
type ScopedLister interface {
	ListScoped(ctx context.Context, org, tenant string, after ListPosition, limit int) ([]ListedSubmission, error)
}

// DeliveryView is one delivery job without personal data, for operators.
type DeliveryView struct {
	Seq          int64
	Namespace    domain.Namespace
	SubmissionID domain.SubmissionID
	ReceivedAt   time.Time
	Erased       bool
	Job          domain.Job
}

// Admin is the operator surface: status without personal data, retry and erase by address.
type Admin interface {
	// Status returns the jobs of up to limit submissions with a sequence above afterSeq, SMTP before Telegram.
	Status(ctx context.Context, afterSeq int64, limit int) ([]DeliveryView, error)
	Retry(ctx context.Context, ns domain.Namespace, id domain.SubmissionID, channel domain.Channel, now time.Time) error
	// EraseEmail erases the personal data of every live submission with the normalized address and returns the count.
	EraseEmail(ctx context.Context, email string, now time.Time) (int, error)
}

// ScopeKind names what a deletion fence covers.
type ScopeKind string

const (
	ScopeRestaurant   ScopeKind = "restaurant"
	ScopeOrganization ScopeKind = "organization"
)

// ScopeDeleted is the fact that Auth deleted a restaurant or organization (event or registry tombstone).
type ScopeDeleted struct {
	Kind      ScopeKind
	ID        string
	EventID   string
	Revision  int64
	DeletedAt time.Time
}

// Lifecycle applies scope deletions. It is idempotent and ignores a revision older than the recorded one.
type Lifecycle interface {
	ApplyScopeDeleted(ctx context.Context, event ScopeDeleted) error
}

// SweepResult counts what one retention pass did.
type SweepResult struct {
	Erased    int
	Deleted   int
	Published int64
	Inbox     int64
}

// Retention erases expired submissions and deletes expired tombstones in bounded batches.
type Retention interface {
	Sweep(ctx context.Context, now time.Time) (SweepResult, error)
}

// Store is everything a storage adapter provides to the service and the tools.
type Store interface {
	Repository
	ScopedRepository
	ScopedLister
	Admin
	Lifecycle
	Retention
}
