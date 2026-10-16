// Source: umir-factory reference/durable@26166c8ba197

package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// maxTxAttempts bounds retries of serialization failures and deadlocks.
	maxTxAttempts = 3

	defaultApplicationName = "umir-service"
	defaultConnectTimeout  = 10 * time.Second
	rollbackTimeout        = 5 * time.Second

	// A retry waits a random time in [retryPauseMin, retryPauseMax] so that
	// the two sides of a conflict do not collide again in lockstep.
	retryPauseMin = 5 * time.Millisecond
	retryPauseMax = 50 * time.Millisecond

	sqlStateSerializationFailure = "40001"
	sqlStateDeadlockDetected     = "40P01"
)

// Config describes how to reach the database.
type Config struct {
	// URL is a libpq URL. Pool tuning may come from its query parameters.
	URL string
	// ApplicationName is reported to the server unless the URL already sets one.
	ApplicationName string
	// MaxConns caps the pool. Zero keeps pool_max_conns from the URL or, when
	// absent, the pgx default.
	MaxConns int32
	// ConnectTimeout bounds the initial connection check. Zero means 10 seconds.
	ConnectTimeout time.Duration
	// Clock is the time source of leases, outbox claims and retention. Nil uses
	// the database clock; tests inject a fake clock here.
	Clock func() time.Time
}

// DB is the shared connection pool.
type DB struct {
	pool  *pgxpool.Pool
	clock func() time.Time
	// sleep replaces the pause between transaction retries; nil waits on a timer.
	sleep func(context.Context, time.Duration) error
}

// Open creates the pool and verifies connectivity. Errors never contain the URL.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	if cfg.URL == "" {
		return nil, errors.New("database URL is empty")
	}
	pgxConfig, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// The driver error can quote parts of the URL.
		return nil, errors.New("database URL is invalid")
	}
	if pgxConfig.ConnConfig.RuntimeParams["application_name"] == "" {
		name := cfg.ApplicationName
		if name == "" {
			name = defaultApplicationName
		}
		pgxConfig.ConnConfig.RuntimeParams["application_name"] = name
	}
	if cfg.MaxConns > 0 {
		pgxConfig.MaxConns = cfg.MaxConns
	}
	timeout := cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = defaultConnectTimeout
	}
	pgxConfig.ConnConfig.ConnectTimeout = timeout
	pool, err := pgxpool.NewWithConfig(ctx, pgxConfig)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return &DB{pool: pool, clock: cfg.Clock}, nil
}

// Close releases every pooled connection.
func (d *DB) Close() { d.pool.Close() }

// Ping reports whether the database answers.
func (d *DB) Ping(ctx context.Context) error { return d.pool.Ping(ctx) }

// Pool exposes the pool for code that needs dedicated connections.
func (d *DB) Pool() *pgxpool.Pool { return d.pool }

// now is the "current time" argument of statements that compare timestamps: the
// injected clock, or nil so that the statement falls back to clock_timestamp().
func (d *DB) now() any {
	if d.clock == nil {
		return nil
	}
	return d.clock().UTC()
}

// CommitUnknownError reports that COMMIT was sent but its outcome is unknown,
// for example because the connection broke while waiting for the answer. The
// transaction may or may not have been applied; it is never retried.
type CommitUnknownError struct{ Err error }

func (e *CommitUnknownError) Error() string {
	return "transaction commit outcome unknown: " + e.Err.Error()
}

func (e *CommitUnknownError) Unwrap() error { return e.Err }

// InTx runs fn in a transaction. Serialization failures (40001) and deadlocks
// (40P01) are retried, up to three attempts in total, after a pause of 5 to 50
// ms with jitter that ends early when ctx is done; fn must be safe to run again.
//
// When COMMIT was sent and its outcome is not known (the connection broke, the
// context ended while the answer was awaited, or the server answered with a
// FATAL/PANIC error or an error of class 08 or 57), the error is a
// *CommitUnknownError. A context that is already done before COMMIT is sent is an
// ordinary error: the transaction is rolled back. A panic in fn rolls the
// transaction back and propagates.
func (d *DB) InTx(ctx context.Context, opts pgx.TxOptions, fn func(context.Context, pgx.Tx) error) error {
	return d.run(ctx, opts, nil, fn)
}

// run is InTx with a hook that prepares every attempt, used to set the scope.
func (d *DB) run(ctx context.Context, opts pgx.TxOptions, prepare, fn func(context.Context, pgx.Tx) error) error {
	return d.retry(ctx, func() error { return d.attempt(ctx, opts, prepare, fn) })
}

// retry calls once up to maxTxAttempts times while it fails with a retryable
// error and ctx is alive, pausing with jitter between the calls.
func (d *DB) retry(ctx context.Context, once func() error) error {
	var err error
	for attempt := 1; attempt <= maxTxAttempts; attempt++ {
		err = once()
		if err == nil || !retryable(err) || ctx.Err() != nil || attempt == maxTxAttempts {
			return err
		}
		if d.pause(ctx) != nil {
			// ctx ended during the pause: report the failure that was being retried.
			return err
		}
	}
	return err
}

// pause waits between two attempts. It returns the context error when ctx ends
// first. Tests replace the wait through DB.sleep.
func (d *DB) pause(ctx context.Context) error {
	delay := retryPauseMin + rand.N(retryPauseMax-retryPauseMin+1) //nolint:gosec // jitter, not security
	if d.sleep != nil {
		return d.sleep(ctx, delay)
	}
	return sleepContext(ctx, delay)
}

// sleepContext waits for delay or until ctx is done, whichever is first.
func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (d *DB) attempt(ctx context.Context, opts pgx.TxOptions, prepare, fn func(context.Context, pgx.Tx) error) error {
	tx, err := d.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	finished := false
	defer func() {
		if finished {
			return
		}
		// fn or prepare panicked: free the connection before the panic goes on.
		rollback(ctx, tx)
	}()
	if prepare != nil {
		if err := prepare(ctx, tx); err != nil {
			finished = true
			rollback(ctx, tx)
			return err
		}
	}
	if err := fn(ctx, tx); err != nil {
		finished = true
		rollback(ctx, tx)
		return err
	}
	finished = true
	if err := ctx.Err(); err != nil {
		// COMMIT was never sent: this is a rollback, not an unknown outcome.
		rollback(ctx, tx)
		return fmt.Errorf("commit transaction: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		if commitOutcomeUnknown(err) {
			return &CommitUnknownError{Err: err}
		}
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// commitOutcomeUnknown classifies an error returned while COMMIT was in flight.
// An ordinary server error (a deferred constraint, a serialization failure) or
// pgx's own transaction state errors mean the transaction did not commit. A
// FATAL or PANIC error and the classes 08 (connection exception) and 57
// (operator intervention) end the session, so the server may or may not have
// committed first; everything that is not a server error (a broken connection, a
// context that ended while waiting) is unknown too.
func commitOutcomeUnknown(err error) bool {
	if errors.Is(err, pgx.ErrTxClosed) || errors.Is(err, pgx.ErrTxCommitRollback) {
		return false
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return true
	}
	if pgErr.Severity == "FATAL" || pgErr.Severity == "PANIC" ||
		pgErr.SeverityUnlocalized == "FATAL" || pgErr.SeverityUnlocalized == "PANIC" {
		return true
	}
	return strings.HasPrefix(pgErr.Code, "08") || strings.HasPrefix(pgErr.Code, "57")
}

// rollback runs on a fresh context so that it proceeds after ctx is cancelled.
// Its error is dropped on purpose: the caller reports the original failure, and a
// connection that cannot roll back is destroyed by the pool.
func rollback(ctx context.Context, tx pgx.Tx) {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if err := tx.Rollback(rollbackCtx); err != nil {
		return
	}
}

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == sqlStateSerializationFailure || pgErr.Code == sqlStateDeadlockDetected
}
