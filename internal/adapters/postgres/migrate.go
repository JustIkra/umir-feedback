// Source: umir-factory reference/durable@26166c8ba197

package postgres

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrSchemaNewer is returned when the database was migrated by a newer build.
	ErrSchemaNewer = errors.New("database schema is newer than this build")
	// ErrSchemaOutdated is returned when the database lacks migrations this
	// build needs. The runtime role cannot apply them; run the migrate job.
	ErrSchemaOutdated = errors.New("database schema is older than this build: run the migrate job")
	// ErrMigrationMismatch is returned when an applied migration does not match
	// the one embedded in this build.
	ErrMigrationMismatch = errors.New("applied migration does not match this build")
)

var (
	migrationFile = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)
	serviceName   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
)

// Spec names the service and its embedded migrations: files Dir/NNNN_name.sql in
// FS, numbered from 1 without gaps. A line "-- umir:include <fragment>" inside a
// file is replaced by a reference fragment (storage_meta, worker_leases, outbox,
// inbox). A line that mentions "umir:include" but is not a well-formed directive
// is an error that names the line.
//
// There is no checksum of applied migrations: schema_migrations records the
// version and the name only, so editing an applied file is not detected. Never
// change a migration that has been applied; add a new one.
type Spec struct {
	// Service names the migration lock: hashtext('<Service>.migrate').
	Service string
	FS      fs.FS
	Dir     string
}

type migration struct {
	version int
	name    string
	sql     string
}

type appliedMigration struct {
	version int
	name    string
}

func loadMigrations(spec Spec) ([]migration, error) {
	if !serviceName.MatchString(spec.Service) {
		return nil, errors.New("migration service name must be lower case letters, digits, '-' or '_'")
	}
	if spec.FS == nil {
		return nil, errors.New("migration file system is nil")
	}
	entries, err := fs.ReadDir(spec.FS, spec.Dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	list := make([]migration, 0, len(entries))
	for _, entry := range entries {
		match := migrationFile.FindStringSubmatch(entry.Name())
		if entry.IsDir() || match == nil {
			return nil, fmt.Errorf("unexpected migration file %q: want NNNN_name.sql", entry.Name())
		}
		version, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", entry.Name(), err)
		}
		body, err := fs.ReadFile(spec.FS, path.Join(spec.Dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		sql, err := expandIncludes(string(body))
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", entry.Name(), err)
		}
		if strings.TrimSpace(sql) == "" {
			return nil, fmt.Errorf("migration %q is empty", entry.Name())
		}
		list = append(list, migration{version: version, name: match[2], sql: sql})
	}
	if len(list) == 0 {
		return nil, errors.New("no migrations found")
	}
	sort.Slice(list, func(i, j int) bool { return list[i].version < list[j].version })
	for i, m := range list {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration versions must be 1..N without gaps or duplicates; found %04d at position %d", m.version, i+1)
		}
	}
	return list, nil
}

// checkApplied compares the applied versions (sorted) with the build's list.
func checkApplied(applied []appliedMigration, list []migration) error {
	for i, a := range applied {
		if a.version > len(list) {
			return fmt.Errorf("%w: database is at version %d, this build knows %d", ErrSchemaNewer, applied[len(applied)-1].version, len(list))
		}
		if a.version != i+1 {
			return fmt.Errorf("%w: version %d is missing from the history", ErrMigrationMismatch, i+1)
		}
		if want := list[a.version-1].name; a.name != want {
			return fmt.Errorf("%w: version %d is %q in the database and %q in this build", ErrMigrationMismatch, a.version, a.name, want)
		}
	}
	return nil
}

func readApplied(ctx context.Context, tx pgx.Tx) ([]appliedMigration, error) {
	rows, err := tx.Query(ctx, `SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	var applied []appliedMigration
	for rows.Next() {
		var a appliedMigration
		if err := rows.Scan(&a.version, &a.name); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied = append(applied, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	return applied, nil
}

// Migrate applies the missing forward-only migrations of spec. Run it with the
// owner role, from the migrate job only. The whole run is one transaction under
// pg_advisory_xact_lock(hashtext('<service>.migrate')), so concurrent runs apply
// each migration once. It refuses a database that is newer than this build
// (ErrSchemaNewer) or whose recorded names differ (ErrMigrationMismatch).
func Migrate(ctx context.Context, db *DB, spec Spec) error {
	list, err := loadMigrations(spec)
	if err != nil {
		return err
	}
	return db.InTx(ctx, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		// The lock comes before CREATE TABLE: two runs on an empty database
		// would otherwise race on the catalog.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, spec.Service+".migrate"); err != nil {
			return fmt.Errorf("lock migrations: %w", err)
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version    integer     PRIMARY KEY,
			name       text        NOT NULL,
			applied_at timestamptz NOT NULL)`); err != nil {
			return fmt.Errorf("create schema_migrations: %w", err)
		}
		applied, err := readApplied(ctx, tx)
		if err != nil {
			return err
		}
		if err := checkApplied(applied, list); err != nil {
			return err
		}
		for _, m := range list[len(applied):] {
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return fmt.Errorf("apply migration %04d_%s: %w", m.version, m.name, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name, applied_at)
				VALUES ($1, $2, COALESCE($3::timestamptz, now()))`, m.version, m.name, db.now()); err != nil {
				return fmt.Errorf("record migration %04d_%s: %w", m.version, m.name, err)
			}
		}
		return nil
	})
}

// VerifySchema is the runtime check: the database must be at exactly the version
// of spec, with matching names. It only reads. A database behind the build is
// ErrSchemaOutdated (the migrate job has not run), one ahead is ErrSchemaNewer.
func VerifySchema(ctx context.Context, db *DB, spec Spec) error {
	list, err := loadMigrations(spec)
	if err != nil {
		return err
	}
	return db.InTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
			return fmt.Errorf("look up schema_migrations: %w", err)
		}
		if !exists {
			return fmt.Errorf("%w: schema_migrations does not exist", ErrSchemaOutdated)
		}
		applied, err := readApplied(ctx, tx)
		if err != nil {
			return err
		}
		if err := checkApplied(applied, list); err != nil {
			return err
		}
		if len(applied) < len(list) {
			return fmt.Errorf("%w: database is at version %d, this build needs %d", ErrSchemaOutdated, len(applied), len(list))
		}
		return nil
	})
}
