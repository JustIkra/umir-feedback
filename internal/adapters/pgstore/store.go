// Package pgstore is the PostgreSQL feedback store. It implements the
// application ports over the copied reference adapter (internal/adapters/postgres):
// the submission and its two delivery jobs, the task alarms and the quota commit
// together, the delivery job row is the truth and the broker message only wakes
// a worker (R18).
package pgstore

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JustIkra/umir-feedback/internal/adapters/postgres"
	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Spec names the embedded migrations for the reference Migrate and VerifySchema.
func Spec() postgres.Spec {
	return postgres.Spec{Service: "feedback", FS: migrationFiles, Dir: "migrations"}
}

const (
	timePrecision = time.Microsecond
	// DefaultLease is how long a claimed delivery may stay in flight before another claim takes it over.
	DefaultLease = 5 * time.Minute
	// wakeAfter is how long a job may wait for its alarm before the reconcile loop sends another.
	wakeAfter    = time.Minute
	sweepBatch   = 500
	maxClaimLoop = 256
	readyTimeout = 2 * time.Second
	// OutboxRetention and InboxRetention are the R9 periods for published outbox rows and inbox records.
	OutboxRetention = 7 * 24 * time.Hour
	InboxRetention  = 14 * 24 * time.Hour
	maxAcceptTries  = 3
)

// Config describes the store.
type Config struct {
	DB        *postgres.DB
	Policy    domain.RetryPolicy
	Lease     time.Duration
	Retention application.RetentionConfig
	Quota     application.QuotaConfig
	// Now is the business clock (Europe/Moscow).
	Now func() time.Time
	// NewID returns a canonical lowercase UUID v4 for jobs and outbox rows.
	NewID func() (string, error)
}

// Guard names the worker lease whose fence a background write must still hold.
type Guard struct {
	Name   string
	Holder string
	Fence  int64
}

// Store implements application.Store on PostgreSQL.
type Store struct {
	db         *postgres.DB
	outbox     *postgres.Outbox
	inbox      *postgres.Inbox
	cfg        Config
	generation string
	moscow     *time.Location
	guard      *Guard
}

var _ application.Store = (*Store)(nil)

// Open verifies that the schema is exactly the one of this build and reads the
// storage generation. It never migrates: that is the job of `feedback migrate`
// under the owner role (C8).
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DB == nil || cfg.Now == nil || cfg.NewID == nil || !cfg.Policy.Valid() {
		return nil, errors.New("pgstore: database, clock, id source and retry policy are required")
	}
	if cfg.Lease == 0 {
		cfg.Lease = DefaultLease
	}
	if cfg.Lease < time.Second || cfg.Lease > 24*time.Hour {
		return nil, errors.New("pgstore: lease must be between 1s and 24h")
	}
	if cfg.Retention.AnonymousDays < 1 || cfg.Retention.RestaurantDays < 1 || cfg.Retention.DeletedScopeDays < 1 || cfg.Retention.TombstoneDays < 1 || cfg.Quota.Anonymous < 1 || cfg.Quota.Restaurant < 1 {
		return nil, errors.New("pgstore: retention periods and quotas must be positive")
	}
	if err := postgres.VerifySchema(ctx, cfg.DB, Spec()); err != nil {
		return nil, err
	}
	generation, err := postgres.Generation(ctx, cfg.DB)
	if err != nil {
		return nil, err
	}
	return &Store{db: cfg.DB, outbox: postgres.NewOutbox(cfg.DB), inbox: postgres.NewInbox(cfg.DB), cfg: cfg, generation: generation, moscow: domain.MoscowLocation()}, nil
}

// Generation is the storage generation read at open (C9).
func (s *Store) Generation() string { return s.generation }

// DB exposes the pool wrapper for the relay and the leases.
func (s *Store) DB() *postgres.DB { return s.db }

// Outbox exposes the outbox for the relay adapter.
func (s *Store) Outbox() *postgres.Outbox { return s.outbox }

// WithGuard returns a view of the store whose background writes (retention, wake,
// scope deletion, registry) first check that the lease fence still holds.
func (s *Store) WithGuard(g Guard) *Store {
	copied := *s
	copied.guard = &g
	return &copied
}

// Ready reports whether the database answers.
func (s *Store) Ready() bool {
	ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
	defer cancel()
	return s.db.Ping(ctx) == nil
}

// Close releases the pool.
func (s *Store) Close() error {
	s.db.Close()
	return nil
}

// Schema reports whether the database schema is the one this build needs.
func (s *Store) Schema(ctx context.Context) error {
	return postgres.VerifySchema(ctx, s.db, Spec())
}

// OutboxStats is the backlog of unpublished task alarms.
func (s *Store) OutboxStats(ctx context.Context) (postgres.OutboxStats, error) {
	return s.outbox.Stats(ctx)
}

// storageError marks a driver failure as storage unavailable and keeps the cause for logs.
func storageError(op string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, application.ErrIdentityConflict), errors.Is(err, application.ErrNotFound), errors.Is(err, application.ErrLeaseLost),
		errors.Is(err, application.ErrQuotaExceeded), errors.Is(err, application.ErrScopeDeleted), errors.Is(err, application.ErrNotRetryable),
		errors.Is(err, application.ErrForbidden), errors.Is(err, application.ErrStorageUnavailable),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return fmt.Errorf("%w: %s: %w", application.ErrStorageUnavailable, op, err)
}

func scopeOf(ns domain.Namespace) postgres.Scope {
	if ns.Kind == "app" {
		return postgres.Scope{Actor: postgres.ActorTenant, OrgID: ns.OrgID, RestaurantID: ns.TenantID, UserID: ns.UserID}
	}
	return postgres.Scope{Actor: postgres.ActorAnonymous}
}

func (s *Store) inScope(ctx context.Context, ns domain.Namespace, opts pgx.TxOptions, fn func(context.Context, pgx.Tx) error) error {
	return s.db.InScopedTx(ctx, scopeOf(ns), opts, fn)
}

func (s *Store) inSystem(ctx context.Context, reason string, opts pgx.TxOptions, fn func(context.Context, pgx.Tx) error) error {
	return s.db.InSystemTx(ctx, reason, opts, func(ctx context.Context, tx pgx.Tx) error {
		if s.guard != nil {
			var held bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM worker_leases
				 WHERE name = $1 AND holder = $2 AND fence = $3 AND expires_at > clock_timestamp())`,
				s.guard.Name, s.guard.Holder, s.guard.Fence).Scan(&held); err != nil {
				return err
			}
			if !held {
				return application.ErrLeaseLost
			}
		}
		return fn(ctx, tx)
	})
}

// ts maps a zero time to NULL and truncates to the column precision.
func ts(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Truncate(timePrecision)
}

func (s *Store) moscowTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.In(s.moscow)
}

const submissionColumns = `s.pk, s.id::text, s.namespace_kind, COALESCE(s.user_id::text, ''), COALESCE(s.org_id::text, ''), COALESCE(s.tenant_id::text, ''),
	COALESCE(s.name, ''), COALESCE(s.email, ''), COALESCE(s.phone, ''), COALESCE(s.message, ''), s.consent_version, s.consent_purpose,
	s.accepted_at, s.received_at, s.fingerprint, s.erased_at IS NOT NULL`

const jobColumns = `j.id::text, j.channel, j.state, j.attempts, j.next_attempt_at, j.retry_started_at, j.started_at, j.confirmed_at, j.error_code, j.provider_id, j.fence`

type scanner interface{ Scan(dest ...any) error }

func (s *Store) scanSubmission(row scanner, extra ...any) (int64, domain.Submission, error) {
	var pk int64
	var sub domain.Submission
	var id, kind, user, org, tenant string
	var accepted, received time.Time
	dest := append([]any{&pk, &id, &kind, &user, &org, &tenant, &sub.Name, &sub.Email, &sub.Phone, &sub.Message, &sub.ConsentVersion, &sub.ConsentPurpose, &accepted, &received, &sub.Fingerprint, &sub.Erased}, extra...)
	if err := row.Scan(dest...); err != nil {
		return 0, domain.Submission{}, err
	}
	sub.ID = domain.SubmissionID(id)
	sub.CommandID = id
	sub.Namespace = domain.Namespace{Kind: kind, UserID: user, OrgID: org, TenantID: tenant}
	sub.AcceptedAt, sub.ReceivedAt = accepted.In(s.moscow), received.In(s.moscow)
	return pk, sub, nil
}

// jobScan holds the destinations of one job row (jobColumns).
type jobScan struct {
	job                             domain.Job
	channel, state                  string
	next, retry, started, confirmed *time.Time
}

func (j *jobScan) dest() []any {
	return []any{&j.job.ID, &j.channel, &j.state, &j.job.Attempts, &j.next, &j.retry, &j.started, &j.confirmed, &j.job.ErrorCode, &j.job.ProviderID, &j.job.Fence}
}

func (s *Store) jobFrom(j *jobScan) domain.Job {
	job := j.job
	job.Channel, job.State = domain.Channel(j.channel), domain.JobState(j.state)
	job.NextAttemptAt, job.RetryStartedAt = s.moscowTime(j.next), s.moscowTime(j.retry)
	job.StartedAt, job.ConfirmedAt = s.moscowTime(j.started), s.moscowTime(j.confirmed)
	return job
}

// updateJob writes the mutable columns of a job.
func updateJob(ctx context.Context, tx pgx.Tx, job domain.Job) error {
	_, err := tx.Exec(ctx, `UPDATE feedback_jobs SET state = $2, attempts = $3, next_attempt_at = $4, retry_started_at = $5,
		started_at = $6, confirmed_at = $7, error_code = $8, provider_id = $9, fence = $10 WHERE id = $1::uuid`,
		job.ID, string(job.State), job.Attempts, ts(job.NextAttemptAt), ts(job.RetryStartedAt), ts(job.StartedAt), ts(job.ConfirmedAt), job.ErrorCode, job.ProviderID, job.Fence)
	return err
}

// keyPredicate selects one submission of a namespace by its public identity. The
// constant namespace_kind lets the planner use the partial unique indexes.
func keyPredicate(ns domain.Namespace, id domain.SubmissionID, first int) (string, []any) {
	if ns.Kind == "app" {
		return fmt.Sprintf("s.namespace_kind = 'app' AND s.user_id = $%d::uuid AND s.org_id = $%d::uuid AND s.tenant_id = $%d::uuid AND s.id = $%d::uuid", first, first+1, first+2, first+3),
			[]any{ns.UserID, ns.OrgID, ns.TenantID, string(id)}
	}
	return fmt.Sprintf("s.namespace_kind = 'anonymous' AND s.id = $%d::uuid", first), []any{string(id)}
}

func (s *Store) Lookup(ctx context.Context, id domain.SubmissionID) (domain.Submission, bool, error) {
	return s.LookupScoped(ctx, domain.Anonymous(), id)
}

func (s *Store) LookupScoped(ctx context.Context, ns domain.Namespace, id domain.SubmissionID) (domain.Submission, bool, error) {
	if !ns.Valid() {
		return domain.Submission{}, false, errors.New("invalid namespace")
	}
	where, args := keyPredicate(ns, id, 1)
	var sub domain.Submission
	found := false
	err := s.inScope(ctx, ns, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		found = false
		_, got, err := s.scanSubmission(tx.QueryRow(ctx, `SELECT `+submissionColumns+` FROM feedback_submissions s WHERE `+where, args...))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		sub, found = got, true
		return nil
	})
	if err != nil {
		return domain.Submission{}, false, storageError("lookup", err)
	}
	return sub, found, nil
}

func (s *Store) ListScoped(ctx context.Context, org, tenant string, after application.ListPosition, limit int) ([]application.ListedSubmission, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("list limit must be between 1 and 1000")
	}
	scope := postgres.Scope{Actor: postgres.ActorTenant, OrgID: org, RestaurantID: tenant}
	afterAt := any("-infinity")
	if !after.ReceivedAt.IsZero() {
		afterAt = after.ReceivedAt.Truncate(timePrecision)
	}
	rows := []application.ListedSubmission{}
	err := s.db.InScopedTx(ctx, scope, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		rows = rows[:0]
		result, err := tx.Query(ctx, `SELECT `+submissionColumns+` FROM feedback_submissions s
			 WHERE s.namespace_kind = 'app' AND s.org_id = $1::uuid AND s.tenant_id = $2::uuid AND s.erased_at IS NULL
			   AND (s.received_at, s.pk) > ($3::timestamptz, $4::bigint)
			 ORDER BY s.received_at, s.pk LIMIT $5`, org, tenant, afterAt, after.Seq, limit)
		if err != nil {
			return err
		}
		defer result.Close()
		for result.Next() {
			pk, sub, err := s.scanSubmission(result)
			if err != nil {
				return err
			}
			rows = append(rows, application.ListedSubmission{Submission: sub, Position: application.ListPosition{ReceivedAt: sub.ReceivedAt, Seq: pk}})
		}
		return result.Err()
	})
	if err != nil {
		return nil, storageError("list", err)
	}
	return rows, nil
}
