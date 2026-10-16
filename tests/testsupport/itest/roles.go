package itest

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Roles is a temporary database with a real owner role and a real runtime
// role, like umir_feedback_owner and umir_feedback in production (C8). The
// runtime role is not the owner and has NOBYPASSRLS. The roles carry random
// names; the feedback migration grants to the role named by the connection
// setting umir.runtime_role (RuntimeOptions), which production leaves unset.
type Roles struct {
	AdminURL    string
	OwnerURL    string
	RuntimeURL  string
	OwnerRole   string
	RuntimeRole string
	password    string
	mu          sync.Mutex
	extra       []string
}

// NewRoles creates both roles and a temporary database; all are removed when
// the test ends (the database first, then the roles).
func NewRoles(t testing.TB) *Roles {
	t.Helper()
	ctx := t.Context()
	r := &Roles{password: randHex(t) + randHex(t)}
	base := "t_feedback_" + randHex(t)
	r.OwnerRole, r.RuntimeRole = base+"_owner", base+"_rt"
	cluster := DatabaseURL(t)
	conn, err := pgx.Connect(ctx, cluster)
	if err != nil {
		t.Fatalf("connect to the cluster: %v", err)
	}
	defer closePG(conn)
	t.Cleanup(func() { r.dropRoles(t, cluster) })
	for _, role := range []string{r.OwnerRole, r.RuntimeRole} {
		r.createRole(t, conn, role, "NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS")
	}
	r.AdminURL = NewDatabase(t)
	admin, err := pgx.Connect(ctx, r.AdminURL)
	if err != nil {
		t.Fatalf("connect to the temporary database: %v", err)
	}
	defer closePG(admin)
	owner, rt := pgx.Identifier{r.OwnerRole}.Sanitize(), pgx.Identifier{r.RuntimeRole}.Sanitize()
	db := pgx.Identifier{databaseName(t, r.AdminURL)}.Sanitize()
	for _, stmt := range []string{
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s, %s", db, owner, rt),
		"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
		fmt.Sprintf("GRANT USAGE, CREATE ON SCHEMA public TO %s", owner),
		fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", rt),
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	r.OwnerURL = withUser(t, r.AdminURL, r.OwnerRole, r.password, "")
	r.RuntimeURL = withUser(t, r.AdminURL, r.RuntimeRole, r.password, "")
	return r
}

// OwnerURLGranting is the owner URL whose session setting tells the migration
// which role receives the runtime grants.
func (r *Roles) OwnerURLGranting() string {
	return withUser(nil, r.AdminURL, r.OwnerRole, r.password, r.RuntimeRole)
}

// LoginRole adds another login role (for negative checks) and returns its URL.
func (r *Roles) LoginRole(t testing.TB, suffix, attributes string) string {
	t.Helper()
	ctx := t.Context()
	role := "t_feedback_" + randHex(t) + "_" + suffix
	conn, err := pgx.Connect(ctx, DatabaseURL(t))
	if err != nil {
		t.Fatalf("connect to the cluster: %v", err)
	}
	defer closePG(conn)
	r.mu.Lock()
	r.extra = append(r.extra, role)
	r.mu.Unlock()
	r.createRole(t, conn, role, attributes)
	admin, err := pgx.Connect(ctx, r.AdminURL)
	if err != nil {
		t.Fatalf("connect to the temporary database: %v", err)
	}
	defer closePG(admin)
	q := pgx.Identifier{role}.Sanitize()
	for _, stmt := range []string{
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", pgx.Identifier{databaseName(t, r.AdminURL)}.Sanitize(), q),
		fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", q),
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return withUser(t, r.AdminURL, role, r.password, "")
}

func (r *Roles) createRole(t testing.TB, conn *pgx.Conn, role, attributes string) {
	t.Helper()
	// The password is random hex produced above, so inlining it is safe.
	stmt := fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s' %s", pgx.Identifier{role}.Sanitize(), r.password, attributes)
	if _, err := conn.Exec(t.Context(), stmt); err != nil {
		t.Fatalf("create role %s: %v", role, err)
	}
}

func (r *Roles) dropRoles(t testing.TB, cluster string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, cluster)
	if err != nil {
		t.Errorf("connect to drop roles: %v", err)
		return
	}
	defer closePG(conn)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, role := range append([]string{r.OwnerRole, r.RuntimeRole}, r.extra...) {
		q := pgx.Identifier{role}.Sanitize()
		if _, err := conn.Exec(ctx, "DROP OWNED BY "+q); err != nil {
			t.Errorf("drop owned by %s: %v", role, err)
		}
		if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+q); err != nil {
			t.Errorf("drop role %s: %v", role, err)
		}
	}
}

// withUser returns raw with the given credentials. A non-empty grantee adds the
// session setting umir.runtime_role to the connection options.
func withUser(tb testing.TB, raw, user, password, grantee string) string {
	if tb != nil {
		tb.Helper()
	}
	u, err := url.Parse(raw)
	if err != nil {
		if tb != nil {
			tb.Fatalf("parse database URL: %v", err)
		}
		return ""
	}
	u.User = url.UserPassword(user, password)
	if grantee != "" {
		q := u.Query()
		q.Set("options", "-c umir.runtime_role="+grantee)
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func databaseName(t testing.TB, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || len(u.Path) < 2 {
		t.Fatalf("parse database URL")
	}
	return u.Path[1:]
}
