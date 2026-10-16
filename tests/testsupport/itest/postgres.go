package itest

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	dbPrefix       = "t_" + repo + "_"
	cleanupTimeout = 30 * time.Second
)

// NewDatabase creates a temporary database t_feedback_<rand> on the server of
// UMIR_TEST_DATABASE_URL and returns the URL of that database. The database is
// dropped (WITH (FORCE)) when the test ends. Only databases created here are
// ever dropped.
func NewDatabase(tb testing.TB) string {
	tb.Helper()
	admin := DatabaseURL(tb)
	name := dbPrefix + randHex(tb)
	ctx, cancel := context.WithTimeout(tb.Context(), cleanupTimeout)
	defer cancel()
	if err := createDatabase(ctx, admin, name); err != nil {
		tb.Fatalf("create test database %s: %v", name, err)
	}
	tb.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer dropCancel()
		if err := dropDatabase(dropCtx, admin, name); err != nil {
			tb.Errorf("drop test database %s: %v", name, err)
		}
	})
	return databaseURLFor(admin, name)
}

func databaseURLFor(admin, name string) string {
	u, err := url.Parse(admin)
	if err != nil {
		// DatabaseURL was already used to connect, so this cannot happen.
		return ""
	}
	u.Path = "/" + name
	u.RawPath = ""
	return u.String()
}

func connectAdmin(ctx context.Context, admin string) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		return nil, fmt.Errorf("connect to the administrative database: %w", err)
	}
	return conn, nil
}

func createDatabase(ctx context.Context, admin, name string) error {
	if !strings.HasPrefix(name, dbPrefix) {
		return fmt.Errorf("refusing to create %q: not a test database name", name)
	}
	conn, err := connectAdmin(ctx, admin)
	if err != nil {
		return err
	}
	defer closePG(conn)
	_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	return err
}

func dropDatabase(ctx context.Context, admin, name string) error {
	if !strings.HasPrefix(name, dbPrefix) {
		return fmt.Errorf("refusing to drop %q: not a test database name", name)
	}
	conn, err := connectAdmin(ctx, admin)
	if err != nil {
		return err
	}
	defer closePG(conn)
	_, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	return err
}

func closePG(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	ignoreErr(conn.Close(ctx))
}
