package pgstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

// WakeDue sends a new alarm for every job that is due (or whose lease ran out)
// and has not been woken for a minute. It protects the work from a lost alarm,
// from one that went to the dead-letter queue and from a crashed worker: the
// job row never depends on one message. It returns the number of alarms.
func (s *Store) WakeDue(ctx context.Context, now time.Time) (int, error) {
	now = now.Truncate(timePrecision)
	count := 0
	err := s.inSystem(ctx, "feedback wake due jobs", pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		count = 0
		rows, err := tx.Query(ctx, `SELECT j.id::text, j.channel, s.id::text
			  FROM feedback_jobs j JOIN feedback_submissions s ON s.pk = j.submission_pk
			 WHERE s.erased_at IS NULL
			   AND ((j.state = 'pending' AND j.next_attempt_at <= $1::timestamptz)
			     OR (j.state = 'in-flight' AND j.started_at <= $1::timestamptz - make_interval(secs => $2::float8)))
			   AND (j.last_wake_at IS NULL OR j.last_wake_at <= $1::timestamptz - make_interval(secs => $3::float8))
			 ORDER BY j.next_attempt_at NULLS LAST, j.id LIMIT $4 FOR UPDATE OF j SKIP LOCKED`,
			now, s.cfg.Lease.Seconds(), wakeAfter.Seconds(), sweepBatch)
		if err != nil {
			return err
		}
		type due struct{ job, channel, submission string }
		var list []due
		for rows.Next() {
			var d due
			if err := rows.Scan(&d.job, &d.channel, &d.submission); err != nil {
				rows.Close()
				return err
			}
			list = append(list, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, d := range list {
			job := domain.Job{ID: d.job, Channel: domain.Channel(d.channel)}
			if err := s.wake(ctx, tx, job, domain.SubmissionID(d.submission), now, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE feedback_jobs SET last_wake_at = $2 WHERE id = $1::uuid`, d.job, now); err != nil {
				return err
			}
		}
		count = len(list)
		return nil
	})
	return count, storageError("wake due jobs", err)
}
