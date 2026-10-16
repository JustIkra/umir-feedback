// Source: umir-factory reference/durable@26166c8ba197

package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Inbox makes the handling of a message idempotent per consumer (inbox.sql): the
// record of the message and its effect commit or roll back together.
type Inbox struct {
	db *DB
}

// NewInbox returns the inbox of db.
func NewInbox(db *DB) *Inbox { return &Inbox{db: db} }

// Apply runs fn once for (consumer, eventID). It inserts the inbox record and
// runs fn in one transaction: when the pair is already there, fn is not called
// and applied is false; when fn fails, nothing is recorded, so a redelivery
// tries again. fn gets the transaction (set a Scope on it with SetScope when it
// writes tenant tables) and must be safe to run again, because a serialization
// failure repeats the transaction. Concurrent deliveries of one message are
// serialized on the inbox record: a delivery that finds the record in a
// transaction that has not finished waits for it. If that transaction commits,
// the waiting one reports applied=false and does not call fn; if it rolls back
// (fn failed), the waiting one finds no record and applies fn itself.
func (i *Inbox) Apply(ctx context.Context, consumer, eventID string, fn func(pgx.Tx) error) (applied bool, err error) {
	if consumer == "" || len(consumer) > maxScopeValueLen || eventID == "" || len(eventID) > maxScopeValueLen {
		return false, errors.New("inbox consumer and event id must be 1..256 characters")
	}
	if fn == nil {
		return false, errors.New("inbox handler is nil")
	}
	err = i.db.InTx(ctx, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		applied = false
		tag, err := tx.Exec(ctx, `INSERT INTO inbox (consumer, message_id, received_at)
			VALUES ($1, $2, COALESCE($3::timestamptz, now()))
			ON CONFLICT (consumer, message_id) DO NOTHING`, consumer, eventID, i.db.now())
		if err != nil {
			return fmt.Errorf("record inbox message: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		if err := fn(tx); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

// DeleteOlderThan deletes up to limit inbox records received more than age ago
// and returns how many. Choose age above the longest redelivery window of the
// broker; after that a repeated message would apply again. Call it until it
// returns less than limit.
func (i *Inbox) DeleteOlderThan(ctx context.Context, age time.Duration, limit int) (int64, error) {
	if age <= 0 {
		return 0, errors.New("retention age must be positive")
	}
	if limit < 1 {
		return 0, errors.New("delete limit must be positive")
	}
	var deleted int64
	err := i.db.InTx(ctx, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM inbox
			 WHERE (consumer, message_id) IN (SELECT consumer, message_id FROM inbox
			               WHERE received_at < COALESCE($1::timestamptz, clock_timestamp()) - make_interval(secs => $2)
			               ORDER BY received_at
			               LIMIT $3
			                 FOR UPDATE SKIP LOCKED)`, i.db.now(), age.Seconds(), limit)
		deleted = tag.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("delete old inbox records: %w", err)
	}
	return deleted, nil
}
