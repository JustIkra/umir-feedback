package pgstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JustIkra/umir-feedback/internal/application"
)

// RegistryRevision is the last Auth registry revision that was fully reconciled.
func (s *Store) RegistryRevision(ctx context.Context) (int64, error) {
	var revision int64
	err := s.inSystem(ctx, "feedback registry revision", pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT revision FROM feedback_registry_state WHERE singleton`).Scan(&revision)
	})
	return revision, storageError("registry revision", err)
}

// ApplyRegistry applies the tombstones of one reconciliation pass and records the
// revision it reached, in one transaction. Fences are recorded for every
// tombstone, also for restaurants that sent nothing yet: a token issued just
// before the deletion must not open a scope that Auth already closed.
func (s *Store) ApplyRegistry(ctx context.Context, revision int64, tombstones []application.ScopeDeleted, now time.Time) error {
	err := s.inSystem(ctx, "feedback registry reconcile", pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		for _, ev := range tombstones {
			if err := validateScopeDeleted(ev); err != nil {
				return err
			}
			if err := s.applyScopeDeleted(ctx, tx, ev); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE feedback_registry_state SET revision = GREATEST(revision, $1), checked_at = $2 WHERE singleton`, revision, now.Truncate(timePrecision))
		return err
	})
	return storageError("registry reconcile", err)
}
