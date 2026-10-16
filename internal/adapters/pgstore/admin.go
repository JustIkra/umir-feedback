package pgstore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

// Status returns the jobs of up to limit submissions above afterSeq. It never
// reads the personal columns.
func (s *Store) Status(ctx context.Context, afterSeq int64, limit int) ([]application.DeliveryView, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("status limit must be between 1 and 1000")
	}
	views := []application.DeliveryView{}
	err := s.inSystem(ctx, "feedback status", pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		views = views[:0]
		rows, err := tx.Query(ctx, `SELECT s.pk, s.id::text, s.namespace_kind, COALESCE(s.user_id::text, ''), COALESCE(s.org_id::text, ''), COALESCE(s.tenant_id::text, ''),
				s.received_at, s.erased_at IS NOT NULL, `+jobColumns+`
			  FROM (SELECT * FROM feedback_submissions WHERE pk > $1 ORDER BY pk LIMIT $2) s
			  JOIN feedback_jobs j ON j.submission_pk = s.pk
			 ORDER BY s.pk, j.channel`, afterSeq, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v application.DeliveryView
			var id, kind, user, org, tenant string
			var received time.Time
			var js jobScan
			if err := rows.Scan(append([]any{&v.Seq, &id, &kind, &user, &org, &tenant, &received, &v.Erased}, js.dest()...)...); err != nil {
				return err
			}
			v.SubmissionID = domain.SubmissionID(id)
			v.Namespace = domain.Namespace{Kind: kind, UserID: user, OrgID: org, TenantID: tenant}
			v.ReceivedAt = received.In(s.moscow)
			v.Job = s.jobFrom(&js)
			views = append(views, v)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, storageError("status", err)
	}
	return views, nil
}

// EraseEmail erases the personal data of every live submission with the address.
func (s *Store) EraseEmail(ctx context.Context, email string, now time.Time) (int, error) {
	now = now.Truncate(timePrecision)
	count := 0
	err := s.inSystem(ctx, "feedback erase by address", pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		count = 0
		rows, err := tx.Query(ctx, `SELECT pk FROM feedback_submissions WHERE email = $1 AND erased_at IS NULL ORDER BY pk FOR UPDATE`, email)
		if err != nil {
			return err
		}
		pks, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		count = len(pks)
		return s.eraseRows(ctx, tx, pks, now)
	})
	if err != nil {
		return 0, storageError("erase by address", err)
	}
	return count, nil
}
