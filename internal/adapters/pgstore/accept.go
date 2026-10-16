package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/JustIkra/umir-feedback/internal/adapters/postgres"
	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

var errRaced = errors.New("pgstore: concurrent insert of the same submission")

// Exchange and routing keys of the delivery alarms (C2).
const (
	ExchangeTasks    = "umir.tasks"
	RoutingKeySMTP   = "feedback.smtp"
	RoutingKeyTG     = "feedback.telegram"
	taskSchema       = 1
	taskOwner        = "feedback"
	headerSchema     = "x-umir-schema"
	headerGeneration = "x-umir-generation"
)

func routingKey(ch domain.Channel) string {
	if ch == domain.Telegram {
		return RoutingKeyTG
	}
	return RoutingKeySMTP
}

// taskMessage is the body of an alarm (task-message.schema.json). It carries
// identifiers only: no address, name, phone or text.
type taskMessage struct {
	MessageID     string `json:"messageID"`
	Owner         string `json:"owner"`
	SchemaVersion int    `json:"schemaVersion"`
	JobID         string `json:"jobID"`
	Channel       string `json:"channel"`
	Generation    string `json:"generation"`
	OccurredAt    string `json:"occurredAt"`
	CorrelationID string `json:"correlationID"`
}

// wake writes the alarm of one job into the outbox in the caller's transaction.
func (s *Store) wake(ctx context.Context, tx pgx.Tx, job domain.Job, submissionID domain.SubmissionID, notBefore, now time.Time) error {
	rowID, err := s.cfg.NewID()
	if err != nil {
		return err
	}
	body, err := json.Marshal(taskMessage{MessageID: rowID, Owner: taskOwner, SchemaVersion: taskSchema, JobID: job.ID, Channel: string(job.Channel),
		Generation: s.generation, OccurredAt: now.UTC().Format(time.RFC3339), CorrelationID: string(submissionID)})
	if err != nil {
		return err
	}
	_, err = s.outbox.AddEvent(ctx, tx, postgres.Event{ID: rowID, Exchange: ExchangeTasks, RoutingKey: routingKey(job.Channel), Body: body, NotBefore: notBefore.UTC(),
		Headers: map[string]string{headerSchema: "1", headerGeneration: s.generation}})
	return err
}

func quotaKey(ns domain.Namespace) string {
	if ns.Kind == "app" {
		return "restaurant:" + ns.TenantID
	}
	return "anonymous"
}

func (s *Store) quotaLimit(ns domain.Namespace) int {
	if ns.Kind == "app" {
		return s.cfg.Quota.Restaurant
	}
	return s.cfg.Quota.Anonymous
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// Accept stores a submission, its two jobs, their alarms and the quota unit in
// one transaction. A repeat returns the original receipt; other content under
// the same identity is a conflict, also after the personal data was erased.
func (s *Store) Accept(ctx context.Context, sub domain.Submission) (domain.Receipt, bool, error) {
	if sub.Namespace.Kind == "" {
		sub.Namespace = domain.Anonymous()
	}
	if sub.CommandID == "" {
		sub.CommandID = string(sub.ID)
	}
	sub.AcceptedAt, sub.ReceivedAt = sub.AcceptedAt.Truncate(timePrecision), sub.ReceivedAt.Truncate(timePrecision)
	if err := domain.ValidateSubmission(sub); err != nil {
		return domain.Receipt{}, false, err
	}
	var lastErr error
	for range maxAcceptTries {
		receipt, created, err := s.acceptOnce(ctx, sub)
		if errors.Is(err, errRaced) {
			lastErr = err
			continue
		}
		return receipt, created, storageError("accept", err)
	}
	return domain.Receipt{}, false, storageError("accept", lastErr)
}

func (s *Store) acceptOnce(ctx context.Context, sub domain.Submission) (domain.Receipt, bool, error) {
	ns := sub.Namespace
	receipt := domain.Receipt{SubmissionID: sub.ID, ReceivedAt: sub.ReceivedAt}
	created := false
	where, args := keyPredicate(ns, sub.ID, 1)
	err := s.inScope(ctx, ns, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		created = false
		receipt.ReceivedAt = sub.ReceivedAt
		var fingerprint string
		var received time.Time
		err := tx.QueryRow(ctx, `SELECT s.fingerprint, s.received_at FROM feedback_submissions s WHERE `+where, args...).Scan(&fingerprint, &received)
		switch {
		case err == nil:
			if fingerprint != sub.Fingerprint {
				return application.ErrIdentityConflict
			}
			receipt.ReceivedAt = received.In(s.moscow)
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if ns.Kind == "app" {
			var fenced bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM feedback_scope_fences
				 WHERE (kind = 'restaurant' AND scope_id = $1::uuid) OR (kind = 'organization' AND scope_id = $2::uuid))`, ns.TenantID, ns.OrgID).Scan(&fenced); err != nil {
				return err
			}
			if fenced {
				return application.ErrScopeDeleted
			}
			if _, err := tx.Exec(ctx, `INSERT INTO feedback_scopes (tenant_id, org_id) VALUES ($1::uuid, $2::uuid) ON CONFLICT (tenant_id) DO NOTHING`, ns.TenantID, ns.OrgID); err != nil {
				return err
			}
		}
		retention := s.cfg.Retention.AnonymousDays
		if ns.Kind == "app" {
			retention = s.cfg.Retention.RestaurantDays
		}
		var pk int64
		err = tx.QueryRow(ctx, `INSERT INTO feedback_submissions (id, namespace_kind, user_id, org_id, tenant_id, name, email, phone, message,
				consent_version, consent_purpose, accepted_at, received_at, fingerprint, purge_at)
			VALUES ($1::uuid, $2, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, NULLIF($5, '')::uuid, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			ON CONFLICT DO NOTHING RETURNING pk`,
			string(sub.ID), ns.Kind, ns.UserID, ns.OrgID, ns.TenantID, sub.Name, sub.Email, sub.Phone, sub.Message,
			sub.ConsentVersion, sub.ConsentPurpose, sub.AcceptedAt, sub.ReceivedAt, sub.Fingerprint, sub.ReceivedAt.Add(days(retention))).Scan(&pk)
		if errors.Is(err, pgx.ErrNoRows) {
			return errRaced
		}
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23503" {
				return application.ErrForbidden
			}
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO feedback_quota AS q (scope_key, live_rows) VALUES ($1, 1)
			ON CONFLICT (scope_key) DO UPDATE SET live_rows = q.live_rows + 1 WHERE q.live_rows < $2`, quotaKey(ns), s.quotaLimit(ns))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return application.ErrQuotaExceeded
		}
		for _, ch := range []domain.Channel{domain.SMTP, domain.Telegram} {
			id, err := s.cfg.NewID()
			if err != nil {
				return err
			}
			job := domain.Job{ID: id, Channel: ch, State: domain.Pending, NextAttemptAt: sub.ReceivedAt}
			if _, err := tx.Exec(ctx, `INSERT INTO feedback_jobs (id, submission_pk, org_id, tenant_id, channel, state, next_attempt_at, last_wake_at)
				VALUES ($1::uuid, $2, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, $5, 'pending', $6, $6)`, id, pk, ns.OrgID, ns.TenantID, string(ch), sub.ReceivedAt); err != nil {
				return err
			}
			if err := s.wake(ctx, tx, job, sub.ID, sub.ReceivedAt, sub.ReceivedAt); err != nil {
				return err
			}
		}
		created = true
		return nil
	})
	if err != nil {
		return domain.Receipt{}, false, err
	}
	return receipt, created, nil
}
