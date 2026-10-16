// Source: umir-factory reference/durable@26166c8ba197

package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Generation returns the storage generation (a UUID in canonical text form) from
// storage_meta. A service that cannot read one does not start (C9).
func Generation(ctx context.Context, db *DB) (string, error) {
	var generation string
	err := db.InTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT generation::text FROM storage_meta`).Scan(&generation)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errors.New("storage generation is missing: storage_meta has no row")
	}
	if err != nil {
		return "", fmt.Errorf("read storage generation: %w", err)
	}
	return generation, nil
}

// RotateGeneration replaces the storage generation by a fresh random UUID and
// returns it. Only the owner role may call it, with the service stopped (the
// `generation rotate --reason <text>` command), for example after a restore from
// backup. The reason is stored with the previous generation.
func RotateGeneration(ctx context.Context, db *DB, reason string) (string, error) {
	if err := validateReason(reason); err != nil {
		return "", errors.New("generation rotation needs a reason: " + err.Error())
	}
	var generation string
	err := db.InTx(ctx, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `UPDATE storage_meta
			   SET previous_generation = generation,
			       generation = gen_random_uuid(),
			       rotated_at = COALESCE($2::timestamptz, now()),
			       rotation_reason = $1
			 WHERE singleton
			RETURNING generation::text`, reason, db.now()).Scan(&generation)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errors.New("storage generation is missing: storage_meta has no row")
	}
	if err != nil {
		return "", fmt.Errorf("rotate storage generation: %w", err)
	}
	return generation, nil
}
