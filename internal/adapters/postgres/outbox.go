// Source: umir-factory reference/durable@26166c8ba197

package postgres

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const maxRoutingFieldLen = 255

// Event is a message to publish, written in the transaction of the business
// change.
type Event struct {
	// ID is the UUID of the outbox row. Empty means the database generates one.
	// The envelope's eventID is a separate field of Body.
	ID string
	// Exchange and RoutingKey address the message on the broker.
	Exchange   string
	RoutingKey string
	// Headers become message headers (x-umir-schema, x-umir-restaurant-id, ...).
	Headers map[string]string
	// Body is a JSON document. It is stored as jsonb, so key order and spacing
	// are not preserved.
	Body []byte
	// OrgID and RestaurantID are the scope of the event; empty means none.
	OrgID        string
	RestaurantID string
	// NotBefore holds the event back until that time. Zero means now.
	NotBefore time.Time
}

// Row is a claimed outbox row, as the relay publishes it.
type Row struct {
	ID         string
	Position   int64
	Exchange   string
	RoutingKey string
	// Headers are the message headers. AddEvent writes strings only, but the
	// column is jsonb and a row written by other means must not block the head
	// of the queue, so Claim converts every value to a string: strings as they
	// are, numbers and booleans in their JSON spelling, objects and arrays as
	// compact JSON, null values are left out. The outbox_headers_object check
	// guarantees that the column holds a JSON object.
	Headers      map[string]string
	Body         []byte
	OrgID        string
	RestaurantID string
	// Attempts counts the claims of this row, this one included.
	Attempts  int
	CreatedAt time.Time
}

// OutboxStats describes the backlog: the readiness input of a relay.
type OutboxStats struct {
	// Pending counts unpublished rows, claimed and held back ones included.
	Pending int64
	// OldestAge is the age of the oldest unpublished row, zero when none.
	OldestAge time.Duration
}

// Outbox is the transactional outbox table (outbox.sql). Its rows carry event
// payloads (body, headers) of every tenant, yet the table has no row level
// security on purpose (C4): the relay claims rows across the tenant boundary and
// publishes them as one stream. The runtime role of the service may therefore
// read and write every row; the table is not reachable by tenant code paths, and
// org_id and restaurant_id are routing metadata, not an access control.
//
// Ordering. Rows are claimed in position order. Events of one aggregate are
// written one transaction after another, so their positions follow commit order.
// Two relays running at once may publish later rows before earlier ones that the
// other holds; when strict order matters, run one relay under a Lease.
type Outbox struct {
	db *DB
}

// NewOutbox returns the outbox of db.
func NewOutbox(db *DB) *Outbox { return &Outbox{db: db} }

func (e Event) validate() error {
	if e.Exchange == "" || len(e.Exchange) > maxRoutingFieldLen || strings.ContainsRune(e.Exchange, 0) {
		return errors.New("event exchange must be 1..255 characters")
	}
	if e.RoutingKey == "" || len(e.RoutingKey) > maxRoutingFieldLen || strings.ContainsRune(e.RoutingKey, 0) {
		return errors.New("event routing key must be 1..255 characters")
	}
	if !json.Valid(e.Body) {
		return errors.New("event body must be a JSON document")
	}
	for k, v := range e.Headers {
		if k == "" || strings.ContainsRune(k, 0) || strings.ContainsRune(v, 0) {
			return errors.New("event headers must have non-empty names and no NUL characters")
		}
	}
	if len(e.OrgID) > maxScopeValueLen || len(e.RestaurantID) > maxScopeValueLen ||
		strings.ContainsRune(e.OrgID, 0) || strings.ContainsRune(e.RestaurantID, 0) {
		return errors.New("event scope identifiers are invalid")
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// AddEvent inserts the event in the caller's transaction, so it exists exactly
// when the business change commits. It returns the id of the row.
func (o *Outbox) AddEvent(ctx context.Context, tx pgx.Tx, ev Event) (string, error) {
	if err := ev.validate(); err != nil {
		return "", err
	}
	headers := ev.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	var notBefore any
	if !ev.NotBefore.IsZero() {
		notBefore = ev.NotBefore.UTC()
	}
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO outbox
			(id, exchange, routing_key, headers, body, org_id, restaurant_id, created_at, not_before)
		VALUES (COALESCE($1::uuid, gen_random_uuid()), $2, $3, $4, $5, $6, $7,
		        COALESCE($8::timestamptz, now()), COALESCE($9::timestamptz, $8::timestamptz, now()))
		RETURNING id::text`,
		nullable(ev.ID), ev.Exchange, ev.RoutingKey, headers, json.RawMessage(ev.Body),
		nullable(ev.OrgID), nullable(ev.RestaurantID), o.db.now(), notBefore).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("add outbox event: %w", err)
	}
	return id, nil
}

// Claim leases up to limit due, unpublished rows to owner for the lease
// duration, oldest position first, skipping rows another relay holds. It
// returns the rows and the fence of this claim; the owner presents owner and
// fence in MarkPublished and Release. A row whose lease expired is claimed
// again, with a higher fence. With nothing due it returns no rows and fence 0.
func (o *Outbox) Claim(ctx context.Context, owner string, limit int, lease time.Duration) ([]Row, int64, error) {
	if owner == "" || len(owner) > maxScopeValueLen {
		return nil, 0, errors.New("claim owner must be 1..256 characters")
	}
	if limit < 1 {
		return nil, 0, errors.New("claim limit must be positive")
	}
	if lease <= 0 {
		return nil, 0, errors.New("claim lease must be positive")
	}
	var rows []Row
	var fence int64
	err := o.db.InTx(ctx, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		rows, fence = nil, 0
		result, err := tx.Query(ctx, `WITH n AS (SELECT COALESCE($4::timestamptz, clock_timestamp()) AS t),
			picked AS (
				SELECT o.id
				  FROM outbox o, n
				 WHERE o.published_at IS NULL
				   AND o.not_before <= n.t
				   AND (o.claim_expires_at IS NULL OR o.claim_expires_at <= n.t)
				 ORDER BY o.position
				 LIMIT $2
				   FOR UPDATE OF o SKIP LOCKED),
			f AS (SELECT nextval('outbox_claim_fence_seq') AS fence)
			UPDATE outbox o
			   SET claimed_by = $1,
			       claim_fence = f.fence,
			       claim_expires_at = n.t + make_interval(secs => $3),
			       attempts = o.attempts + 1
			  FROM picked, n, f
			 WHERE o.id = picked.id
			RETURNING o.id::text, o.position, o.exchange, o.routing_key, o.headers, o.body,
			          o.org_id, o.restaurant_id, o.attempts, o.created_at, f.fence`,
			owner, limit, lease.Seconds(), o.db.now())
		if err != nil {
			return err
		}
		defer result.Close()
		for result.Next() {
			var r Row
			var org, restaurant *string
			var headers []byte
			if err := result.Scan(&r.ID, &r.Position, &r.Exchange, &r.RoutingKey, &headers, &r.Body,
				&org, &restaurant, &r.Attempts, &r.CreatedAt, &fence); err != nil {
				return err
			}
			var err error
			if r.Headers, err = decodeHeaders(headers); err != nil {
				return fmt.Errorf("outbox row %s: %w", r.ID, err)
			}
			if org != nil {
				r.OrgID = *org
			}
			if restaurant != nil {
				r.RestaurantID = *restaurant
			}
			rows = append(rows, r)
		}
		return result.Err()
	})
	if err != nil {
		return nil, 0, fmt.Errorf("claim outbox rows: %w", err)
	}
	slices.SortFunc(rows, func(a, b Row) int { return cmp.Compare(a.Position, b.Position) })
	return rows, fence, nil
}

// MarkPublished records that the rows were published; routed says whether the
// broker routed them to a queue. Only the holder of the claim (owner and fence)
// may do it, and it may repeat the call: a row already marked by the same claim
// stays as it is. It does not check the expiry: the message is out, and the
// record is right as long as no other relay claimed the row. If any row now
// belongs to another claim, the rest is still recorded and the result is
// ErrLeaseLost.
func (o *Outbox) MarkPublished(ctx context.Context, owner string, fence int64, ids []string, routed bool) error {
	unique := slices.Compact(slices.Sorted(slices.Values(ids)))
	if len(unique) == 0 {
		return nil
	}
	var updated int64
	err := o.db.InTx(ctx, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE outbox
			   SET published_at = COALESCE(published_at, COALESCE($4::timestamptz, now())),
			       routed = COALESCE(routed, $5)
			 WHERE id = ANY($3::uuid[]) AND claimed_by = $1 AND claim_fence = $2`,
			owner, fence, unique, o.db.now(), routed)
		updated = tag.RowsAffected()
		return err
	})
	if err != nil {
		return fmt.Errorf("mark outbox rows published: %w", err)
	}
	if updated != int64(len(unique)) {
		return ErrLeaseLost
	}
	return nil
}

// Release returns claimed, unpublished rows to the queue, to be claimed again
// after delay. A relay uses it when a publication failed or was returned and must
// be retried. Like MarkPublished it needs the owner and fence of the claim and
// reports ErrLeaseLost when any row belongs to another claim.
func (o *Outbox) Release(ctx context.Context, owner string, fence int64, ids []string, delay time.Duration) error {
	unique := slices.Compact(slices.Sorted(slices.Values(ids)))
	if len(unique) == 0 {
		return nil
	}
	if delay < 0 {
		return errors.New("release delay must not be negative")
	}
	var updated int64
	err := o.db.InTx(ctx, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE outbox
			   SET claimed_by = NULL, claim_fence = NULL, claim_expires_at = NULL,
			       not_before = COALESCE($4::timestamptz, clock_timestamp()) + make_interval(secs => $5)
			 WHERE id = ANY($3::uuid[]) AND claimed_by = $1 AND claim_fence = $2 AND published_at IS NULL`,
			owner, fence, unique, o.db.now(), delay.Seconds())
		updated = tag.RowsAffected()
		return err
	})
	if err != nil {
		return fmt.Errorf("release outbox rows: %w", err)
	}
	if updated != int64(len(unique)) {
		return ErrLeaseLost
	}
	return nil
}

// DeleteOlderThan deletes up to limit rows that were published more than age
// ago and returns how many. Unpublished rows are never deleted. Call it until
// it returns less than limit.
func (o *Outbox) DeleteOlderThan(ctx context.Context, age time.Duration, limit int) (int64, error) {
	if age <= 0 {
		return 0, errors.New("retention age must be positive")
	}
	if limit < 1 {
		return 0, errors.New("delete limit must be positive")
	}
	var deleted int64
	err := o.db.InTx(ctx, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM outbox
			 WHERE id IN (SELECT id FROM outbox
			               WHERE published_at IS NOT NULL
			                 AND published_at < COALESCE($1::timestamptz, clock_timestamp()) - make_interval(secs => $2)
			               ORDER BY published_at
			               LIMIT $3
			                 FOR UPDATE SKIP LOCKED)`, o.db.now(), age.Seconds(), limit)
		deleted = tag.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("delete published outbox rows: %w", err)
	}
	return deleted, nil
}

// Stats reports the backlog for readiness.
func (o *Outbox) Stats(ctx context.Context) (OutboxStats, error) {
	var pending int64
	var age float64
	err := o.db.InTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*),
			       COALESCE(EXTRACT(EPOCH FROM (COALESCE($1::timestamptz, clock_timestamp()) - min(created_at))), 0)::float8
			  FROM outbox WHERE published_at IS NULL`, o.db.now()).Scan(&pending, &age)
	})
	if err != nil {
		return OutboxStats{}, fmt.Errorf("read outbox stats: %w", err)
	}
	return OutboxStats{Pending: pending, OldestAge: time.Duration(age * float64(time.Second))}, nil
}

// decodeHeaders turns the jsonb headers into string headers (see Row.Headers).
// Numbers keep their JSON spelling; they are not rounded through float64.
func decodeHeaders(raw []byte) (map[string]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var values map[string]any
	if err := dec.Decode(&values); err != nil {
		return nil, fmt.Errorf("headers are not a JSON object: %w", err)
	}
	if values == nil {
		return nil, errors.New("headers are not a JSON object: null")
	}
	headers := make(map[string]string, len(values))
	for name, value := range values {
		switch v := value.(type) {
		case nil:
			continue
		case string:
			headers[name] = v
		case json.Number:
			headers[name] = v.String()
		case bool:
			headers[name] = strconv.FormatBool(v)
		default: // object or array
			text, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("header %q: %w", name, err)
			}
			headers[name] = string(text)
		}
	}
	return headers, nil
}
