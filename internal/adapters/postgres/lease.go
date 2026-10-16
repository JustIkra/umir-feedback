// Source: umir-factory reference/durable@26166c8ba197

package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Lease is a named lease on worker_leases for background work of a service with
// a database (C4). One holder owns a name at a time. Every new term raises
// fence, so the work it guards can reject a stale owner by comparing fences.
type Lease struct {
	db     *DB
	name   string
	holder string
	ttl    time.Duration

	mu    sync.Mutex
	fence int64
}

// NewLease prepares a lease. holder identifies this process and must differ
// between replicas.
func NewLease(db *DB, name, holder string, ttl time.Duration) (*Lease, error) {
	if name == "" || len(name) > maxScopeValueLen {
		return nil, errors.New("lease name must be 1..256 characters")
	}
	if holder == "" || len(holder) > maxScopeValueLen {
		return nil, errors.New("lease holder must be 1..256 characters")
	}
	if ttl <= 0 {
		return nil, errors.New("lease ttl must be positive")
	}
	return &Lease{db: db, name: name, holder: holder, ttl: ttl}, nil
}

// Fence returns the fence of the term this lease last acquired, or 0.
func (l *Lease) Fence() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fence
}

// Acquire takes the lease when it is free, expired or already ours. It returns
// the fence and true, or false when another holder has it. Re-acquiring a live
// lease of our own extends it and keeps the fence; taking it after expiry or
// release starts a new term with a higher fence.
func (l *Lease) Acquire(ctx context.Context) (int64, bool, error) {
	var fence int64
	err := l.db.InTx(ctx, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		fence = 0
		err := tx.QueryRow(ctx, `INSERT INTO worker_leases AS w (name, holder, fence, expires_at)
			VALUES ($1, $2, 1, COALESCE($3::timestamptz, clock_timestamp()) + make_interval(secs => $4))
			ON CONFLICT (name) DO UPDATE
			   SET holder = EXCLUDED.holder,
			       fence = CASE WHEN w.holder = EXCLUDED.holder
			                     AND w.expires_at > COALESCE($3::timestamptz, clock_timestamp())
			                    THEN w.fence ELSE w.fence + 1 END,
			       expires_at = EXCLUDED.expires_at
			 WHERE w.holder = ''
			    OR w.holder = EXCLUDED.holder
			    OR w.expires_at <= COALESCE($3::timestamptz, clock_timestamp())
			RETURNING fence`, l.name, l.holder, l.db.now(), l.ttl.Seconds()).Scan(&fence)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // held by someone else
		}
		return err
	})
	if err != nil {
		return 0, false, fmt.Errorf("acquire lease: %w", err)
	}
	if fence == 0 {
		return 0, false, nil
	}
	l.mu.Lock()
	l.fence = fence
	l.mu.Unlock()
	return fence, true, nil
}

// Renew extends the term. It fails with ErrLeaseLost when the lease expired,
// even if nobody took it meanwhile, or when another holder has it.
func (l *Lease) Renew(ctx context.Context) error {
	fence := l.Fence()
	var renewed int64
	err := l.db.InTx(ctx, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE worker_leases
			   SET expires_at = COALESCE($4::timestamptz, clock_timestamp()) + make_interval(secs => $5)
			 WHERE name = $1 AND holder = $2 AND fence = $3
			   AND expires_at > COALESCE($4::timestamptz, clock_timestamp())`,
			l.name, l.holder, fence, l.db.now(), l.ttl.Seconds())
		renewed = tag.RowsAffected()
		return err
	})
	if err != nil {
		return fmt.Errorf("renew lease: %w", err)
	}
	if fence == 0 || renewed == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Release gives the lease up. Releasing a lease that someone else holds, that
// expired or that was never held changes nothing and is not an error. The row
// stays, so the next term continues the fence.
func (l *Lease) Release(ctx context.Context) error {
	fence := l.Fence()
	if fence == 0 {
		return nil
	}
	err := l.db.InTx(ctx, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE worker_leases
			   SET holder = '', expires_at = '-infinity'
			 WHERE name = $1 AND holder = $2 AND fence = $3`, l.name, l.holder, fence)
		return err
	})
	if err != nil {
		return fmt.Errorf("release lease: %w", err)
	}
	return nil
}
