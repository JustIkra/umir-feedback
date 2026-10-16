// Source: umir-factory reference/durable@26166c8ba197

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
)

// Names of the transaction-local settings that carry the row-level security
// context (C5). Services have no settings of their own.
const (
	GUCActor        = "umir.actor"
	GUCOrgID        = "umir.org_id"
	GUCRestaurantID = "umir.restaurant_id"
	GUCUserID       = "umir.user_id"
	// GUCReason records why a system transaction runs. Policies do not read it;
	// it is there for triggers and audit.
	GUCReason = "umir.reason"
)

// Actor is the kind of caller a transaction acts for.
type Actor string

// The values of umir.actor.
const (
	ActorTenant    Actor = "tenant"
	ActorUser      Actor = "user"
	ActorAnonymous Actor = "anonymous"
	ActorSystem    Actor = "system"
)

const (
	maxScopeValueLen = 256
	maxReasonLen     = 128
)

// Scope is the RLS context of a transaction. Empty identifiers are stored as
// empty settings, which the policy expressions read as NULL (no match).
type Scope struct {
	Actor        Actor
	OrgID        string
	RestaurantID string
	UserID       string
}

// Validate checks the actor and the identifiers it needs:
//
//   - tenant needs OrgID; RestaurantID narrows it to one restaurant, UserID is optional;
//   - user needs UserID;
//   - anonymous carries no identifiers;
//   - system is not a scope: use InSystemTx.
//
// Messages never contain the values.
func (s Scope) Validate() error {
	switch s.Actor {
	case ActorTenant:
		if s.OrgID == "" {
			return errors.New("tenant scope needs an organization")
		}
	case ActorUser:
		if s.UserID == "" {
			return errors.New("user scope needs a user")
		}
	case ActorAnonymous:
		if s.OrgID != "" || s.RestaurantID != "" || s.UserID != "" {
			return errors.New("anonymous scope carries no identifiers")
		}
	case ActorSystem:
		return errors.New("the system actor is set by InSystemTx, not by a scope")
	default:
		return errors.New("unknown actor")
	}
	for name, v := range map[string]string{"organization": s.OrgID, "restaurant": s.RestaurantID, "user": s.UserID} {
		if len(v) > maxScopeValueLen || strings.ContainsRune(v, 0) {
			return fmt.Errorf("invalid %s identifier", name)
		}
	}
	return nil
}

func validateReason(reason string) error {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		return errors.New("a system transaction needs a reason")
	}
	if len(reason) > maxReasonLen {
		return errors.New("the reason is too long")
	}
	for _, r := range reason {
		if unicode.IsControl(r) {
			return errors.New("the reason contains control characters")
		}
	}
	return nil
}

const setContextSQL = `SELECT set_config('umir.actor', $1, true),
       set_config('umir.org_id', $2, true),
       set_config('umir.restaurant_id', $3, true),
       set_config('umir.user_id', $4, true),
       set_config('umir.reason', $5, true)`

// SetScope sets the context for the rest of tx; it ends with the transaction.
// Code that applies a message of a known scope inside Inbox.Apply calls it.
func SetScope(ctx context.Context, tx pgx.Tx, s Scope) error {
	if err := s.Validate(); err != nil {
		return err
	}
	return setContext(ctx, tx, s, "")
}

// SetSystem makes tx a system transaction (see InSystemTx).
func SetSystem(ctx context.Context, tx pgx.Tx, reason string) error {
	if err := validateReason(reason); err != nil {
		return err
	}
	return setContext(ctx, tx, Scope{Actor: ActorSystem}, reason)
}

func setContext(ctx context.Context, tx pgx.Tx, s Scope, reason string) error {
	if _, err := tx.Exec(ctx, setContextSQL, string(s.Actor), s.OrgID, s.RestaurantID, s.UserID, reason); err != nil {
		return fmt.Errorf("set transaction context: %w", err)
	}
	return nil
}

// InScopedTx is InTx with the scope set inside the transaction, again on every
// retry. An invalid scope fails before the database is touched.
func (d *DB) InScopedTx(ctx context.Context, s Scope, opts pgx.TxOptions, fn func(context.Context, pgx.Tx) error) error {
	if err := s.Validate(); err != nil {
		return err
	}
	return d.run(ctx, opts, func(ctx context.Context, tx pgx.Tx) error {
		return setContext(ctx, tx, s, "")
	}, fn)
}

// InSystemTx is InTx for work that crosses tenants, such as relays and
// schedulers: umir.actor is system and the reference policies let it through.
// The reason is required and is kept in umir.reason for the transaction.
func (d *DB) InSystemTx(ctx context.Context, reason string, opts pgx.TxOptions, fn func(context.Context, pgx.Tx) error) error {
	if err := validateReason(reason); err != nil {
		return err
	}
	return d.run(ctx, opts, func(ctx context.Context, tx pgx.Tx) error {
		return setContext(ctx, tx, Scope{Actor: ActorSystem}, reason)
	}, fn)
}
