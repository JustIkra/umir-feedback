// Source: umir-factory reference/durable@26166c8ba197

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

const runtimeRoleSQL = `SELECT r.rolname, r.rolsuper, r.rolbypassrls,
       EXISTS (SELECT 1
                 FROM pg_class c
                 JOIN pg_namespace n ON n.oid = c.relnamespace
                WHERE n.nspname = ANY (current_schemas(false))
                  AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f')
                  AND pg_has_role(r.oid, c.relowner, 'MEMBER')),
       EXISTS (SELECT 1
                 FROM unnest(current_schemas(false)) AS s
                WHERE has_schema_privilege(r.oid, s, 'CREATE'))
  FROM pg_roles r
 WHERE r.rolname = current_user`

// AssertRuntimeRole fails unless the connected role is fit for the service at
// run time (C8): not a superuser, NOBYPASSRLS, not the owner of (or a member of
// the owner of) any relation in the search path, and without CREATE on those
// schemas. Every service calls it at start, so an owner URL in the wrong place
// stops the service instead of silently weakening the policies.
func AssertRuntimeRole(ctx context.Context, db *DB) error {
	var role string
	var superuser, bypass, owner, create bool
	err := db.InTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, runtimeRoleSQL).Scan(&role, &superuser, &bypass, &owner, &create)
	})
	if err != nil {
		return fmt.Errorf("inspect database role: %w", err)
	}
	var problems []string
	if superuser {
		problems = append(problems, "is a superuser")
	}
	if bypass {
		problems = append(problems, "bypasses row level security")
	}
	if owner {
		problems = append(problems, "owns tables (or is a member of their owner)")
	}
	if create {
		problems = append(problems, "can create objects in the schema")
	}
	if len(problems) > 0 {
		return errors.New("database role " + role + " is not a runtime role: it " + strings.Join(problems, ", "))
	}
	return nil
}
