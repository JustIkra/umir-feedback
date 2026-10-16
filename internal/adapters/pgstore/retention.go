package pgstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JustIkra/umir-feedback/internal/application"
)

// eraseRows turns the submissions into digest tombstones: the personal columns
// go, identity, receipt and the keyed fingerprint stay, the quota unit is freed
// and jobs that could still run end as failed.
func (s *Store) eraseRows(ctx context.Context, tx pgx.Tx, pks []int64, now time.Time) error {
	if len(pks) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `WITH erased AS (
			UPDATE feedback_submissions SET name = NULL, email = NULL, phone = NULL, message = NULL,
			       erased_at = $2, tombstone_until = $3
			 WHERE pk = ANY($1) AND erased_at IS NULL
			RETURNING namespace_kind, tenant_id),
		     freed AS (
			SELECT CASE WHEN namespace_kind = 'app' THEN 'restaurant:' || tenant_id::text ELSE 'anonymous' END AS scope_key, count(*) AS n
			  FROM erased GROUP BY 1)
		UPDATE feedback_quota q SET live_rows = GREATEST(q.live_rows - freed.n, 0) FROM freed WHERE q.scope_key = freed.scope_key`,
		pks, now, now.Add(days(s.cfg.Retention.TombstoneDays)).Truncate(timePrecision)); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE feedback_jobs SET state = 'failed', error_code = 'retention_expired', next_attempt_at = NULL,
			confirmed_at = GREATEST($2::timestamptz, COALESCE(started_at, $2::timestamptz))
		 WHERE submission_pk = ANY($1) AND state IN ('pending', 'in-flight')`, pks, now)
	return err
}

// Sweep erases submissions past their purge time, deletes expired tombstones
// and clears published outbox rows and old inbox records, in bounded batches.
// A submission with a running attempt waits until its lease is over.
func (s *Store) Sweep(ctx context.Context, now time.Time) (application.SweepResult, error) {
	now = now.Truncate(timePrecision)
	var result application.SweepResult
	err := s.inSystem(ctx, "feedback retention", pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		result = application.SweepResult{}
		rows, err := tx.Query(ctx, `SELECT s.pk FROM feedback_submissions s
			 WHERE s.erased_at IS NULL AND s.purge_at <= $1::timestamptz
			   AND NOT EXISTS (SELECT 1 FROM feedback_jobs j WHERE j.submission_pk = s.pk AND j.state = 'in-flight'
			                      AND j.started_at > $1::timestamptz - make_interval(secs => $2::float8))
			 ORDER BY s.purge_at, s.pk LIMIT $3 FOR UPDATE OF s SKIP LOCKED`, now, s.cfg.Lease.Seconds(), sweepBatch)
		if err != nil {
			return err
		}
		pks, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		if err := s.eraseRows(ctx, tx, pks, now); err != nil {
			return err
		}
		result.Erased = len(pks)
		tag, err := tx.Exec(ctx, `DELETE FROM feedback_submissions WHERE pk IN (
				SELECT pk FROM feedback_submissions WHERE erased_at IS NOT NULL AND tombstone_until <= $1::timestamptz
				 ORDER BY tombstone_until, pk LIMIT $2 FOR UPDATE SKIP LOCKED)`, now, sweepBatch)
		if err != nil {
			return err
		}
		result.Deleted = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return application.SweepResult{}, storageError("sweep", err)
	}
	published, err := s.outbox.DeleteOlderThan(ctx, OutboxRetention, sweepBatch)
	if err != nil {
		return result, storageError("sweep outbox", err)
	}
	inbox, err := s.inbox.DeleteOlderThan(ctx, InboxRetention, sweepBatch)
	if err != nil {
		return result, storageError("sweep inbox", err)
	}
	result.Published, result.Inbox = published, inbox
	return result, nil
}

// RecountQuota recomputes the live rows per namespace from the submissions (hourly safety net).
func (s *Store) RecountQuota(ctx context.Context) error {
	err := s.inSystem(ctx, "feedback quota recount", pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO feedback_quota AS q (scope_key, live_rows)
			SELECT CASE WHEN namespace_kind = 'app' THEN 'restaurant:' || tenant_id::text ELSE 'anonymous' END, count(*)
			  FROM feedback_submissions WHERE erased_at IS NULL GROUP BY 1
			ON CONFLICT (scope_key) DO UPDATE SET live_rows = EXCLUDED.live_rows`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE feedback_quota q SET live_rows = 0
			 WHERE live_rows > 0 AND NOT EXISTS (SELECT 1 FROM feedback_submissions s WHERE s.erased_at IS NULL
			         AND CASE WHEN s.namespace_kind = 'app' THEN 'restaurant:' || s.tenant_id::text ELSE 'anonymous' END = q.scope_key)`)
		return err
	})
	return storageError("quota recount", err)
}
