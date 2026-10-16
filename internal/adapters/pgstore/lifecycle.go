package pgstore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/JustIkra/umir-feedback/internal/adapters/postgres"
	"github.com/JustIkra/umir-feedback/internal/application"
)

// ApplyScopeDeleted records the deletion fence and shortens retention.
func (s *Store) ApplyScopeDeleted(ctx context.Context, ev application.ScopeDeleted) error {
	if err := validateScopeDeleted(ev); err != nil {
		return err
	}
	err := s.inSystem(ctx, "feedback scope deleted", pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return s.applyScopeDeleted(ctx, tx, ev)
	})
	return storageError("scope deleted", err)
}

// ApplyScopeDeletedOnce does the same at most once per event ID, recording the
// event in the inbox in the same transaction (consumer feedback.auth). It
// reports whether this call applied the event.
func (s *Store) ApplyScopeDeletedOnce(ctx context.Context, consumer string, ev application.ScopeDeleted) (bool, error) {
	if err := validateScopeDeleted(ev); err != nil {
		return false, err
	}
	applied, err := s.inbox.Apply(ctx, consumer, ev.EventID, func(tx pgx.Tx) error {
		if err := postgres.SetSystem(ctx, tx, "feedback scope deleted"); err != nil {
			return err
		}
		return s.applyScopeDeleted(ctx, tx, ev)
	})
	return applied, storageError("scope deleted event", err)
}

func validateScopeDeleted(ev application.ScopeDeleted) error {
	if ev.Kind != application.ScopeRestaurant && ev.Kind != application.ScopeOrganization {
		return errors.New("unknown scope kind")
	}
	if ev.ID == "" || ev.EventID == "" || ev.DeletedAt.IsZero() {
		return errors.New("scope deletion needs an id, an event id and a time")
	}
	return nil
}

// applyScopeDeleted is idempotent. An event older than the recorded revision is ignored.
func (s *Store) applyScopeDeleted(ctx context.Context, tx pgx.Tx, ev application.ScopeDeleted) error {
	deletedAt := ev.DeletedAt.Truncate(timePrecision)
	var kept bool
	err := tx.QueryRow(ctx, `INSERT INTO feedback_scope_fences AS f (kind, scope_id, deleted_at, event_id, revision)
			VALUES ($1, $2::uuid, $3, $4, $5)
			ON CONFLICT (kind, scope_id) DO UPDATE SET deleted_at = LEAST(f.deleted_at, EXCLUDED.deleted_at), event_id = EXCLUDED.event_id, revision = EXCLUDED.revision
			 WHERE f.revision <= EXCLUDED.revision
			RETURNING true`, string(ev.Kind), ev.ID, deletedAt, ev.EventID, ev.Revision).Scan(&kept)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	column := "tenant_id"
	if ev.Kind == application.ScopeOrganization {
		column = "org_id"
	}
	limit := deletedAt.Add(days(s.cfg.Retention.DeletedScopeDays))
	if _, err := tx.Exec(ctx, `UPDATE feedback_submissions SET purge_at = LEAST(purge_at, $2::timestamptz)
		 WHERE namespace_kind = 'app' AND erased_at IS NULL AND `+column+` = $1::uuid`, ev.ID, limit); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE feedback_jobs SET state = 'failed', error_code = 'scope_deleted', next_attempt_at = NULL,
			confirmed_at = GREATEST($2::timestamptz, COALESCE(started_at, $2::timestamptz))
		 WHERE state = 'pending' AND `+column+` = $1::uuid`, ev.ID, deletedAt)
	return err
}
