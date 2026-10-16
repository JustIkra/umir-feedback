//go:build integration

package pgintegration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/JustIkra/umir-feedback/internal/adapters/pgstore"
	"github.com/JustIkra/umir-feedback/internal/adapters/postgres"
	"github.com/JustIkra/umir-feedback/tests/storecontract"
	"github.com/JustIkra/umir-feedback/tests/testsupport/itest"
)

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func (e *env) admin(t testing.TB, sql string, args ...any) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), e.roles.AdminURL)
	if err != nil {
		t.Fatalf("connect as administrator: %v", err)
	}
	defer func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("administrator statement: %v", err)
	}
}

func (e *env) adminInt(t testing.TB, sql string, args ...any) int {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), e.roles.AdminURL)
	if err != nil {
		t.Fatalf("connect as administrator: %v", err)
	}
	defer func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	var n int
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("administrator query: %v", err)
	}
	return n
}

func TestMigrationsAreIdempotentAndConcurrent(t *testing.T) {
	roles := itest.NewRoles(t)
	e := &env{roles: roles}
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db := e.openDB(t, roles.OwnerURLGranting(), 2)
			errs <- postgres.Migrate(context.Background(), db, pgstore.Spec())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent migrate: %v", err)
		}
	}
	if n := e.adminInt(t, `SELECT count(*) FROM schema_migrations`); n != 1 {
		t.Fatalf("migrations applied %d times", n)
	}
	if n := e.adminInt(t, `SELECT count(*) FROM storage_meta`); n != 1 {
		t.Fatalf("storage generation rows: %d", n)
	}
	owner := e.openDB(t, roles.OwnerURLGranting(), 1)
	if err := postgres.Migrate(context.Background(), owner, pgstore.Spec()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestServiceRefusesAnOutdatedOrNewerSchema(t *testing.T) {
	roles := itest.NewRoles(t)
	e := &env{roles: roles}
	db := e.openDB(t, roles.RuntimeURL, 2)
	_, err := pgstore.Open(context.Background(), e.config(t, db, storecontract.Settings{}))
	if !errors.Is(err, postgres.ErrSchemaOutdated) {
		t.Fatalf("an unmigrated database: %v", err)
	}
	owner := e.openDB(t, roles.OwnerURLGranting(), 2)
	if err := postgres.Migrate(context.Background(), owner, pgstore.Spec()); err != nil {
		t.Fatal(err)
	}
	e.admin(t, `INSERT INTO schema_migrations (version, name, applied_at) VALUES (2, 'from_the_future', now())`)
	_, err = pgstore.Open(context.Background(), e.config(t, db, storecontract.Settings{}))
	if !errors.Is(err, postgres.ErrSchemaNewer) {
		t.Fatalf("a newer schema: %v", err)
	}
}

func TestRuntimeRoleIsNotAnOwnerAndCannotChangeTheSchema(t *testing.T) {
	e := newEnv(t)
	rt := e.openDB(t, e.roles.RuntimeURL, 2)
	if err := postgres.AssertRuntimeRole(context.Background(), rt); err != nil {
		t.Fatalf("runtime role: %v", err)
	}
	if err := postgres.AssertRuntimeRole(context.Background(), e.openDB(t, e.roles.OwnerURL, 1)); err == nil {
		t.Fatal("the owner role must be rejected at start")
	}
	if err := postgres.AssertRuntimeRole(context.Background(), e.openDB(t, e.roles.AdminURL, 1)); err == nil {
		t.Fatal("a superuser must be rejected at start")
	}
	for _, stmt := range []string{
		"CREATE TABLE intruder (id int)",
		"DROP TABLE feedback_jobs",
		"ALTER TABLE feedback_submissions DISABLE ROW LEVEL SECURITY",
		"ALTER TABLE feedback_submissions NO FORCE ROW LEVEL SECURITY",
		"DROP POLICY feedback_submissions_scope ON feedback_submissions",
		"TRUNCATE feedback_submissions",
		"UPDATE storage_meta SET generation = gen_random_uuid()",
		"UPDATE schema_migrations SET name = 'x'",
		"INSERT INTO storage_meta (singleton, generation) VALUES (false, gen_random_uuid())",
	} {
		err := rt.InTx(context.Background(), pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		if err == nil || sqlState(err) != "42501" {
			t.Errorf("%q: want insufficient_privilege, got %v", stmt, err)
		}
	}
	// The generation is readable: the service must know it.
	if _, err := postgres.Generation(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
}

func TestRowLevelSecurityIsolatesScopesOnARealRole(t *testing.T) {
	e := newEnv(t)
	st := storecontract.Settings{}
	store := e.store(t, st)
	now := moscow(time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC))
	const (
		org1, org2 = "123e4567-e89b-42d3-a456-426614174201", "123e4567-e89b-42d3-a456-426614174202"
		t1, t2     = "123e4567-e89b-42d3-a456-426614174301", "123e4567-e89b-42d3-a456-426614174302"
		user       = "123e4567-e89b-42d3-a456-426614174100"
	)
	ns := func(org, tenant string) domainNamespace {
		return domainNamespace{Kind: "app", UserID: user, OrgID: org, TenantID: tenant}
	}
	seed(t, store, ns(org1, t1), "123e4567-e89b-42d3-a456-426614174001", now)
	seed(t, store, ns(org2, t2), "123e4567-e89b-42d3-a456-426614174002", now)
	seed(t, store, domainNamespace{Kind: "anonymous"}, "123e4567-e89b-42d3-a456-426614174003", now)

	// One connection only: every transaction below reuses the same session.
	db := e.openDB(t, e.roles.RuntimeURL, 1)
	count := func(scope postgres.Scope, system bool) int {
		var n int
		fn := func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM feedback_submissions`).Scan(&n)
		}
		var err error
		if system {
			err = db.InSystemTx(context.Background(), "test", pgx.TxOptions{}, fn)
		} else {
			err = db.InScopedTx(context.Background(), scope, pgx.TxOptions{}, fn)
		}
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count(postgres.Scope{Actor: postgres.ActorTenant, OrgID: org1, RestaurantID: t1}, false); got != 1 {
		t.Fatalf("restaurant one sees %d rows", got)
	}
	if got := count(postgres.Scope{Actor: postgres.ActorTenant, OrgID: org1, RestaurantID: t2}, false); got != 0 {
		t.Fatalf("a restaurant of another organization sees %d rows", got)
	}
	if got := count(postgres.Scope{Actor: postgres.ActorAnonymous}, false); got != 1 {
		t.Fatalf("anonymous sees %d rows", got)
	}
	if got := count(postgres.Scope{}, true); got != 3 {
		t.Fatalf("system sees %d rows", got)
	}
	// Outside any transaction wrapper there is no context: no rows, and nothing leaked from the transactions above.
	var bare int
	if err := db.Pool().QueryRow(context.Background(), `SELECT count(*) FROM feedback_submissions`).Scan(&bare); err != nil || bare != 0 {
		t.Fatalf("a bare connection sees %d rows (%v)", bare, err)
	}
	var leaked string
	if err := db.Pool().QueryRow(context.Background(), `SELECT COALESCE(current_setting('umir.actor', true), '')`).Scan(&leaked); err != nil || leaked != "" {
		t.Fatalf("context leaked through the pool: %q %v", leaked, err)
	}
	// A tenant cannot plant a row for another restaurant, nor change jobs of another scope.
	err := db.InScopedTx(context.Background(), postgres.Scope{Actor: postgres.ActorTenant, OrgID: org1, RestaurantID: t1}, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO feedback_submissions (id, namespace_kind, user_id, org_id, tenant_id, name, email, consent_version, consent_purpose, accepted_at, received_at, fingerprint, purge_at)
			VALUES (gen_random_uuid(), 'app', $1::uuid, $2::uuid, $3::uuid, 'n', 'e@example.test', 'v', 'p', now(), now(), repeat('a', 64), now())`, user, org2, t2)
		return err
	})
	if err == nil || sqlState(err) != "42501" {
		t.Fatalf("planting a foreign row: %v", err)
	}
	var changed int64
	err = db.InScopedTx(context.Background(), postgres.Scope{Actor: postgres.ActorTenant, OrgID: org1, RestaurantID: t1}, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE feedback_jobs SET state = 'failed' WHERE tenant_id <> $1::uuid OR tenant_id IS NULL`, t1)
		changed = tag.RowsAffected()
		return err
	})
	if err != nil || changed != 0 {
		t.Fatalf("a tenant changed %d foreign jobs (%v)", changed, err)
	}
	// The scope of a job must equal the scope of its submission.
	err = db.InSystemTx(context.Background(), "test", pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE feedback_jobs SET tenant_id = $1::uuid WHERE tenant_id = $2::uuid`, t2, t1)
		return err
	})
	if err == nil || sqlState(err) != "23503" {
		t.Fatalf("a job with a foreign tenant: %v", err)
	}
	// A restaurant belongs to one organization.
	err = db.InSystemTx(context.Background(), "test", pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO feedback_submissions (id, namespace_kind, user_id, org_id, tenant_id, name, email, consent_version, consent_purpose, accepted_at, received_at, fingerprint, purge_at)
			VALUES (gen_random_uuid(), 'app', $1::uuid, $2::uuid, $3::uuid, 'n', 'e@example.test', 'v', 'p', now(), now(), repeat('a', 64), now())`, user, org2, t1)
		return err
	})
	if err == nil || sqlState(err) != "23503" {
		t.Fatalf("a restaurant under two organizations: %v", err)
	}
}

func TestAnAnonymousTokenCannotWriteAnAppRow(t *testing.T) {
	e := newEnv(t)
	db := e.openDB(t, e.roles.RuntimeURL, 1)
	err := db.InScopedTx(context.Background(), postgres.Scope{Actor: postgres.ActorAnonymous}, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO feedback_submissions (id, namespace_kind, name, email, consent_version, consent_purpose, accepted_at, received_at, fingerprint, purge_at)
			VALUES (gen_random_uuid(), 'anonymous', 'n', 'e@example.test', 'v', 'p', now(), now(), repeat('a', 64), now())`)
		return err
	})
	if err != nil {
		t.Fatalf("an anonymous row for the anonymous actor: %v", err)
	}
	err = db.InScopedTx(context.Background(), postgres.Scope{Actor: postgres.ActorTenant, OrgID: "123e4567-e89b-42d3-a456-426614174201", RestaurantID: "123e4567-e89b-42d3-a456-426614174301"}, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO feedback_submissions (id, namespace_kind, name, email, consent_version, consent_purpose, accepted_at, received_at, fingerprint, purge_at)
			VALUES (gen_random_uuid(), 'anonymous', 'n', 'e@example.test', 'v', 'p', now(), now(), repeat('a', 64), now())`)
		return err
	})
	if err == nil || sqlState(err) != "42501" {
		t.Fatalf("a tenant writing an anonymous row: %v", err)
	}
}
