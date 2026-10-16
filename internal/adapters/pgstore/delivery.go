package pgstore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

func exhausted(job domain.Job, now time.Time) domain.Job {
	job.State = domain.Failed
	job.ErrorCode = "retry_exhausted"
	job.NextAttemptAt = time.Time{}
	job.ConfirmedAt = now
	return job
}

// Claim leases the oldest due job of the channel. A job whose lease expired is
// requeued first, a job that ran out of retries is failed. The claim raises the
// fence of the job: only the holder of the latest fence can finish it.
func (s *Store) Claim(ctx context.Context, channel domain.Channel, now time.Time, policy domain.RetryPolicy) (domain.Delivery, bool, error) {
	if !domain.ValidChannel(channel) || !policy.Valid() {
		return domain.Delivery{}, false, errors.New("invalid delivery channel or retry policy")
	}
	if !domain.IsMoscowTime(now) {
		return domain.Delivery{}, false, errors.New("claim time must use Europe/Moscow")
	}
	now = now.Truncate(timePrecision)
	var claimed domain.Delivery
	var found bool
	err := s.inSystem(ctx, "feedback claim", pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		claimed, found = domain.Delivery{}, false
		for range maxClaimLoop {
			row := tx.QueryRow(ctx, `SELECT `+submissionColumns+`, `+jobColumns+`
				  FROM feedback_jobs j JOIN feedback_submissions s ON s.pk = j.submission_pk
				 WHERE j.channel = $1 AND s.erased_at IS NULL AND (
				       (j.state = 'pending' AND (j.next_attempt_at <= $2::timestamptz OR j.attempts >= $3
				            OR $2::timestamptz >= COALESCE(j.retry_started_at, s.received_at) + make_interval(secs => $4::float8)))
				    OR (j.state = 'in-flight' AND j.started_at <= $2::timestamptz - make_interval(secs => $5::float8)))
				 ORDER BY s.received_at, s.pk
				 FOR UPDATE OF j SKIP LOCKED LIMIT 1`, string(channel), now, policy.MaxAttempts, policy.MaxAge.Seconds(), s.cfg.Lease.Seconds())
			_, sub, job, err := s.scanClaim(row)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			before := job
			cycle := domain.RetryCycleStart(job, sub)
			switch {
			case job.State == domain.InFlight:
				if policy.Exhausted(job.Attempts, cycle, now) {
					job = exhausted(job, now)
				} else {
					job.State = domain.Pending
					job.NextAttemptAt = job.StartedAt.Add(policy.Delay(job.Attempts))
				}
			case policy.Exhausted(job.Attempts, cycle, now):
				job = exhausted(job, now)
			case job.NextAttemptAt.After(now):
				return nil
			default:
				if err := tx.QueryRow(ctx, `SELECT nextval('feedback_job_fence_seq')`).Scan(&job.Fence); err != nil {
					return err
				}
				job.State = domain.InFlight
				job.Attempts++
				job.StartedAt = now
				job.NextAttemptAt = time.Time{}
				job.ConfirmedAt = time.Time{}
				job.ErrorCode = ""
				job.ProviderID = ""
			}
			if !domain.ValidTransition(before, job) || !domain.ValidJobTimeline(job, sub) {
				return errors.New("stored delivery job cannot make a valid transition")
			}
			if err := updateJob(ctx, tx, job); err != nil {
				return err
			}
			if job.State == domain.InFlight {
				claimed, found = domain.Delivery{Submission: sub, Job: job}, true
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return domain.Delivery{}, false, storageError("claim", err)
	}
	return claimed, found, nil
}

func (s *Store) scanClaim(row scanner) (int64, domain.Submission, domain.Job, error) {
	var js jobScan
	pk, sub, err := s.scanSubmission(row, js.dest()...)
	if err != nil {
		return 0, domain.Submission{}, domain.Job{}, err
	}
	return pk, sub, s.jobFrom(&js), nil
}

func (s *Store) Finish(ctx context.Context, id domain.SubmissionID, channel domain.Channel, job domain.Job) error {
	return s.FinishScoped(ctx, domain.Anonymous(), id, channel, job)
}

// FinishScoped records the result of an attempt. The job must still be
// in flight under the fence the caller holds; otherwise another claim took it
// over and the result is dropped with ErrLeaseLost.
func (s *Store) FinishScoped(ctx context.Context, ns domain.Namespace, id domain.SubmissionID, channel domain.Channel, job domain.Job) error {
	if !ns.Valid() {
		return errors.New("invalid namespace")
	}
	if !domain.ValidChannel(channel) {
		return errors.New("invalid delivery state transition")
	}
	job.NextAttemptAt, job.RetryStartedAt = job.NextAttemptAt.Truncate(timePrecision), job.RetryStartedAt.Truncate(timePrecision)
	job.StartedAt, job.ConfirmedAt = job.StartedAt.Truncate(timePrecision), job.ConfirmedAt.Truncate(timePrecision)
	where, args := keyPredicate(ns, id, 2)
	err := s.inSystem(ctx, "feedback finish", pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		pk, sub, current, err := s.scanClaim(tx.QueryRow(ctx, `SELECT `+submissionColumns+`, `+jobColumns+`
			  FROM feedback_jobs j JOIN feedback_submissions s ON s.pk = j.submission_pk
			 WHERE j.channel = $1 AND `+where+` FOR UPDATE OF j`, append([]any{string(channel)}, args...)...))
		if errors.Is(err, pgx.ErrNoRows) {
			return application.ErrNotFound
		}
		if err != nil {
			return err
		}
		next := job
		if next.ID == "" {
			next.ID = current.ID
		}
		if next.ID != current.ID {
			return errors.New("job identity mismatch")
		}
		if sub.Erased {
			return application.ErrNotFound
		}
		if current.State != domain.InFlight || next.Fence != current.Fence {
			return application.ErrLeaseLost
		}
		if next.Channel != channel || !domain.ValidFinish(current, next) || !domain.ValidJobTimeline(next, sub) {
			return errors.New("invalid delivery state transition")
		}
		if next.State == domain.Pending && ns.Kind == "app" {
			var fenced bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM feedback_scope_fences
				 WHERE (kind = 'restaurant' AND scope_id = $1::uuid) OR (kind = 'organization' AND scope_id = $2::uuid))`, ns.TenantID, ns.OrgID).Scan(&fenced); err != nil {
				return err
			}
			if fenced {
				next.State = domain.Failed
				next.ErrorCode = "scope_deleted"
				next.NextAttemptAt = time.Time{}
				next.ConfirmedAt = s.cfg.Now().Truncate(timePrecision)
				if next.ConfirmedAt.Before(next.StartedAt) {
					next.ConfirmedAt = next.StartedAt
				}
			}
		}
		next.Fence = current.Fence
		if err := updateJob(ctx, tx, next); err != nil {
			return err
		}
		if next.State == domain.Pending {
			// The alarm is held back until the retry time; the row stays the truth.
			if _, err := tx.Exec(ctx, `UPDATE feedback_jobs SET last_wake_at = $2 WHERE id = $1::uuid`, next.ID, next.NextAttemptAt); err != nil {
				return err
			}
			return s.wake(ctx, tx, next, sub.ID, next.NextAttemptAt, s.cfg.Now())
		}
		if next.State == domain.Sent {
			_, err := tx.Exec(ctx, `UPDATE feedback_submissions SET message = NULL, phone = NULL, payload_cleared_at = $2
				 WHERE pk = $1 AND payload_cleared_at IS NULL
				   AND (SELECT count(*) FROM feedback_jobs WHERE submission_pk = $1 AND state = 'sent') = 2`, pk, s.cfg.Now().Truncate(timePrecision))
			return err
		}
		return nil
	})
	return storageError("finish", err)
}

// Retry reopens a failed delivery of any namespace with a fresh retry cycle.
func (s *Store) Retry(ctx context.Context, ns domain.Namespace, id domain.SubmissionID, channel domain.Channel, now time.Time) error {
	if !ns.Valid() || !domain.ValidChannel(channel) {
		return application.ErrNotRetryable
	}
	if !domain.IsMoscowTime(now) {
		return errors.New("retry time must use Europe/Moscow")
	}
	now = now.Truncate(timePrecision)
	where, args := keyPredicate(ns, id, 2)
	err := s.inSystem(ctx, "feedback retry", pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, sub, current, err := s.scanClaim(tx.QueryRow(ctx, `SELECT `+submissionColumns+`, `+jobColumns+`
			  FROM feedback_jobs j JOIN feedback_submissions s ON s.pk = j.submission_pk
			 WHERE j.channel = $1 AND `+where+` FOR UPDATE OF j`, append([]any{string(channel)}, args...)...))
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && sub.Erased) {
			return application.ErrNotFound
		}
		if err != nil {
			return err
		}
		if current.State != domain.Failed {
			return application.ErrNotRetryable
		}
		if ns.Kind == "app" {
			var fenced bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM feedback_scope_fences
				 WHERE (kind = 'restaurant' AND scope_id = $1::uuid) OR (kind = 'organization' AND scope_id = $2::uuid))`, ns.TenantID, ns.OrgID).Scan(&fenced); err != nil {
				return err
			}
			if fenced {
				return application.ErrNotRetryable
			}
		}
		next := current
		next.State = domain.Pending
		next.Attempts = 0
		next.NextAttemptAt = now
		next.RetryStartedAt = now
		next.StartedAt = time.Time{}
		next.ConfirmedAt = time.Time{}
		next.ErrorCode = ""
		next.ProviderID = ""
		if domain.ValidateJob(next) != nil || !domain.ValidTransition(current, next) || !domain.ValidJobTimeline(next, sub) {
			return errors.New("delivery job timeline is invalid")
		}
		if err := updateJob(ctx, tx, next); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE feedback_jobs SET last_wake_at = $2 WHERE id = $1::uuid`, next.ID, now); err != nil {
			return err
		}
		return s.wake(ctx, tx, next, sub.ID, now, now)
	})
	return storageError("retry", err)
}
