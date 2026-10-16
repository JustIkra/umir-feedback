// Source: umir-factory reference/durable@26166c8ba197

package relay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults of a Relay whose field is zero.
const (
	DefaultBatch          = 100
	DefaultLease          = 30 * time.Second
	DefaultPoll           = 200 * time.Millisecond // C48: p95 delay of a committed event stays under 1 s
	DefaultRetryBase      = 500 * time.Millisecond
	DefaultRetryMax       = 30 * time.Second
	DefaultStoreTimeout   = 5 * time.Second
	defaultPublishTimeout = 10 * time.Second
	maxBackoffShift       = 20
	// maxBrokerPause caps the shared pause after Nacked or Unknown.
	maxBrokerPause = 5 * time.Second
)

var (
	// ErrLeaseLost is what an OutboxStore returns (possibly wrapped) when the
	// claim of the rows no longer belongs to this relay: the lease expired and
	// another relay took the rows over.
	ErrLeaseLost = errors.New("relay: lease lost")
	// ErrRunning: Run is already active on this Relay.
	ErrRunning = errors.New("relay: already running")
)

// Row is a claimed outbox row. The fields mirror postgres.Row.
type Row struct {
	ID         string
	Position   int64
	Exchange   string
	RoutingKey string
	// Headers are the message headers stored with the event (x-umir-schema,
	// x-umir-restaurant-id, x-umir-generation).
	Headers      map[string]string
	Body         []byte
	OrgID        string
	RestaurantID string
	// Attempts counts the claims of this row, this one included.
	Attempts  int
	CreatedAt time.Time
}

// Backlog describes the unpublished rows. The fields mirror postgres.OutboxStats.
type Backlog struct {
	// Pending counts unpublished rows, claimed and held back ones included.
	Pending int64
	// OldestAge is the age of the oldest unpublished row, zero when none.
	OldestAge time.Duration
}

// OutboxStore is the outbox table as the relay uses it (postgres.Outbox with a
// thin adapter). Errors that mean "the claim is no longer yours" must wrap
// ErrLeaseLost.
type OutboxStore interface {
	// Claim leases up to limit due rows to owner and returns them, oldest
	// position first, with the fence of this claim. No due rows is no rows.
	Claim(ctx context.Context, owner string, limit int, lease time.Duration) ([]Row, int64, error)
	// MarkPublished records the rows as published; routed says whether the broker
	// routed them to a queue. It wraps ErrLeaseLost when a row belongs to another claim.
	MarkPublished(ctx context.Context, owner string, fence int64, ids []string, routed bool) error
	// Release returns claimed, unpublished rows to the queue, claimable again after delay.
	Release(ctx context.Context, owner string, fence int64, ids []string, delay time.Duration) error
	// Stats reports the backlog for readiness.
	Stats(ctx context.Context) (Backlog, error)
}

// Outcome is what is known about a published message. The values and meaning
// are those of rabbitmq.Outcome.
type Outcome int

const (
	// Unknown: the result is not known (connection lost, timeout, nothing
	// sent). It is the zero value, so an unset Outcome is never taken for success.
	Unknown Outcome = iota
	// Confirmed: the broker routed the message to a queue and confirmed it.
	Confirmed
	// Returned: no queue was bound (mandatory return); the message is stored nowhere.
	Returned
	// Nacked: the broker refused the message; it is stored nowhere.
	Nacked
)

func (o Outcome) String() string {
	switch o {
	case Confirmed:
		return "confirmed"
	case Returned:
		return "returned"
	case Nacked:
		return "nacked"
	default:
		return "unknown"
	}
}

// Message is one message to publish. The fields, their order and their types
// are those of rabbitmq.Message, so the adapter converts it directly.
type Message struct {
	Exchange   string
	RoutingKey string
	// MessageID is the AMQP message_id: eventID of an event, messageID of a task.
	MessageID string
	// Type is the AMQP type: the routing key.
	Type string
	// Schema is the header x-umir-schema.
	Schema int
	// RestaurantID is the header x-umir-restaurant-id; none when empty.
	RestaurantID string
	// Generation is the header x-umir-generation.
	Generation string
	Body       []byte
}

// Publisher publishes one message and waits for the broker's verdict
// (rabbitmq.Publisher with a thin adapter). The error is non-nil exactly when
// the outcome is Unknown.
type Publisher interface {
	Publish(ctx context.Context, msg Message) (Outcome, error)
}

// Stats is the state of a relay for readiness and metrics: the backlog of the
// store and the counters of this Relay since it was created.
type Stats struct {
	Backlog
	// Published counts rows marked published, unrouted ones included.
	Published int64
	// ReturnedAccepted counts rows the broker returned and the Accept policy
	// marked published with routed=false (C45).
	ReturnedAccepted int64
	// ReturnedRetried counts returns the Retry policy handed back to the outbox.
	ReturnedRetried int64
	Nacked          int64
	Unknown         int64
	// Invalid counts rows that cannot become a message (ErrInvalidRow).
	Invalid int64
	// LeaseLost counts batches stopped because the claim passed to another relay.
	LeaseLost int64
	// LeaseExpiring counts batches ended early because the lease was running out.
	LeaseExpiring int64
	// StoreErrors counts failed Claim, MarkPublished and Release calls.
	StoreErrors int64
}

// Relay publishes outbox rows. Fill the exported fields before Run and do not
// change them afterwards; copy a Relay only before first use.
type Relay struct {
	Store OutboxStore
	Pub   Publisher
	// Owner names this relay in claims; it must differ between relays that share
	// an outbox (the replica ID).
	Owner string
	// Batch is the number of rows per claim, DefaultBatch when zero.
	Batch int
	// Lease is how long a claim holds its rows, DefaultLease when zero. Rows of a
	// batch that the relay did not reach before the lease runs out are handed back.
	Lease time.Duration
	// Poll is the wait between claims when the outbox is idle, DefaultPoll when
	// zero. Notify cuts it short.
	Poll time.Duration
	// Policy chooses what a mandatory return means; nil uses DefaultPolicies.
	Policy PolicyFunc
	// PublishTimeout bounds one Publish; the smaller of 10 s and a third of the
	// lease when zero.
	PublishTimeout time.Duration
	// StoreTimeout bounds MarkPublished and Release, which run even when Run's
	// context has ended so that a confirmed message is recorded. DefaultStoreTimeout when zero.
	StoreTimeout time.Duration
	// RetryBase and RetryMax shape the delay of a returned row: RetryBase doubled
	// per attempt up to RetryMax. They also bound the wait after store errors.
	RetryBase time.Duration
	RetryMax  time.Duration
	// Now and Sleep replace the clock and the wait between claims in tests. Sleep
	// returns the error of ctx when it ends; when set it replaces the wait and
	// Notify has no effect.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	// Logger receives events without message bodies; nil discards them.
	Logger *slog.Logger

	running  atomic.Bool
	wakeOnce sync.Once
	wake     chan struct{}

	published, returnedAccepted, returnedRetried atomic.Int64
	nacked, unknown, invalid                     atomic.Int64
	leaseLost, leaseExpiring, storeErrors        atomic.Int64
}

// errBroker ends a cycle after Nacked or Unknown: Run then waits with a growing
// pause instead of claiming the next rows at once, so a dead broker costs no
// claim storm and later events of an aggregate do not overtake the held-back one.
var errBroker = errors.New("relay: the broker did not confirm a message")

func (r *Relay) batchSize() int {
	if r.Batch > 0 {
		return r.Batch
	}
	return DefaultBatch
}

func (r *Relay) lease() time.Duration {
	if r.Lease > 0 {
		return r.Lease
	}
	return DefaultLease
}

func (r *Relay) poll() time.Duration {
	if r.Poll > 0 {
		return r.Poll
	}
	return DefaultPoll
}

func (r *Relay) publishTimeout() time.Duration {
	if r.PublishTimeout > 0 {
		return r.PublishTimeout
	}
	return min(defaultPublishTimeout, r.lease()/3)
}

func (r *Relay) storeTimeout() time.Duration {
	if r.StoreTimeout > 0 {
		return r.StoreTimeout
	}
	return DefaultStoreTimeout
}

func (r *Relay) retryBase() time.Duration {
	if r.RetryBase > 0 {
		return r.RetryBase
	}
	return DefaultRetryBase
}

func (r *Relay) retryMax() time.Duration {
	if r.RetryMax > 0 {
		return r.RetryMax
	}
	return DefaultRetryMax
}

func (r *Relay) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Relay) log() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (r *Relay) policy(exchange, key string) ReturnPolicy {
	if r.Policy != nil {
		return r.Policy(exchange, key)
	}
	return DefaultPolicies().PolicyFor(exchange, key)
}

// backoff is the delay before a row that failed on its attempt-th claim is tried again.
func (r *Relay) backoff(attempt int) time.Duration {
	shift := min(max(attempt-1, 0), maxBackoffShift)
	d := r.retryBase() << shift
	if d <= 0 || d > r.retryMax() {
		return r.retryMax()
	}
	return d
}

func (r *Relay) wakeChan() chan struct{} {
	r.wakeOnce.Do(func() { r.wake = make(chan struct{}, 1) })
	return r.wake
}

// Notify wakes the relay at once, for example after a transaction that added an
// event committed in the same process. It never blocks and may be called before
// Run; calls made while a wake-up is pending coalesce.
func (r *Relay) Notify() {
	select {
	case r.wakeChan() <- struct{}{}:
	default:
	}
}

func (r *Relay) wait(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	case <-r.wakeChan():
		return nil
	}
}

// Stats returns the backlog of the store and the counters of this relay. A store
// error is returned together with the counters.
func (r *Relay) Stats(ctx context.Context) (Stats, error) {
	st := Stats{
		Published:        r.published.Load(),
		ReturnedAccepted: r.returnedAccepted.Load(),
		ReturnedRetried:  r.returnedRetried.Load(),
		Nacked:           r.nacked.Load(),
		Unknown:          r.unknown.Load(),
		Invalid:          r.invalid.Load(),
		LeaseLost:        r.leaseLost.Load(),
		LeaseExpiring:    r.leaseExpiring.Load(),
		StoreErrors:      r.storeErrors.Load(),
	}
	backlog, err := r.Store.Stats(ctx)
	if err != nil {
		return st, fmt.Errorf("read outbox backlog: %w", err)
	}
	st.Backlog = backlog
	return st, nil
}

// Run claims and publishes batches until ctx ends and returns ctx.Err(). It owns
// no goroutine: it works in the caller's goroutine and returns only when
// everything it started is finished. A claimed row that was not published is
// handed back before Run returns. A second concurrent Run returns ErrRunning.
func (r *Relay) Run(ctx context.Context) error {
	if r.Store == nil || r.Pub == nil || r.Owner == "" {
		return errors.New("relay: Store, Pub and Owner are required")
	}
	if !r.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	defer r.running.Store(false)
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		before := r.published.Load()
		drained, err := r.cycle(ctx)
		confirmed := r.published.Load() > before
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay := r.poll()
		switch {
		case err != nil:
			failures++
			limit := r.retryMax()
			if errors.Is(err, errBroker) {
				limit = min(limit, maxBrokerPause)
			}
			delay = min(delay<<min(failures-1, maxBackoffShift), limit)
			if delay <= 0 {
				delay = limit
			}
			r.log().Warn("relay cycle failed", "error", err.Error(), "retry_in", delay)
		case drained:
			failures = 0
			continue
		default:
			// Only a confirmed row ends a run of failures: an empty claim says
			// nothing about the broker.
			if confirmed {
				failures = 0
			}
		}
		if err := r.wait(ctx, delay); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// storeCtx returns a context for a write that must happen even after ctx ended:
// the confirmed message has to be recorded and unpublished rows handed back.
func (r *Relay) storeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), r.storeTimeout())
}

// cycle claims one batch and works through it. drained reports that the batch
// was full and fully done, so more rows may be waiting. A non-nil error is a
// store failure or errBroker, after which Run waits longer, or the error of ctx.
func (r *Relay) cycle(ctx context.Context) (drained bool, err error) {
	rows, fence, err := r.Store.Claim(ctx, r.Owner, r.batchSize(), r.lease())
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		r.storeErrors.Add(1)
		return false, fmt.Errorf("claim outbox rows: %w", err)
	}
	if len(rows) == 0 {
		return false, nil
	}
	claimedAt := r.now()
	slices.SortStableFunc(rows, func(a, b Row) int {
		switch {
		case a.Position < b.Position:
			return -1
		case a.Position > b.Position:
			return 1
		}
		return 0
	})
	for i, row := range rows {
		rest := rows[i:]
		if ctx.Err() != nil {
			r.release(ctx, fence, rest, 0)
			return false, ctx.Err()
		}
		if r.now().Add(r.publishTimeout()).After(claimedAt.Add(r.lease())) {
			// A publish started now could outlive the lease, and another relay
			// might publish the row too.
			r.leaseExpiring.Add(1)
			r.release(ctx, fence, rest, 0)
			return false, nil
		}
		msg, err := buildMessage(row)
		if err != nil {
			r.invalid.Add(1)
			r.log().Error("relay holds back an invalid outbox row", "row", row.ID, "exchange", row.Exchange, "key", row.RoutingKey, "error", err.Error())
			r.release(ctx, fence, rows[i:i+1], r.retryMax())
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, r.publishTimeout())
		out, perr := r.Pub.Publish(pctx, msg)
		cancel()
		switch out {
		case Confirmed:
			if stop, err := r.mark(ctx, fence, row, true, rest[1:]); stop {
				return false, err
			}
		case Returned:
			if r.policy(row.Exchange, row.RoutingKey) == Accept {
				if stop, err := r.mark(ctx, fence, row, false, rest[1:]); stop {
					return false, err
				}
				r.returnedAccepted.Add(1)
				r.log().Warn("relay accepted a returned message", "row", row.ID, "exchange", row.Exchange, "key", row.RoutingKey)
				continue
			}
			r.returnedRetried.Add(1)
			r.log().Error("relay: message returned, no queue is bound; it will be retried", "row", row.ID, "exchange", row.Exchange, "key", row.RoutingKey)
			r.release(ctx, fence, rows[i:i+1], r.backoff(row.Attempts))
		default: // Nacked, Unknown, and anything the publisher invents
			if out == Unknown && ctx.Err() != nil {
				r.release(ctx, fence, rest, 0) // shutdown, not a broker failure
				return false, ctx.Err()
			}
			if out == Nacked {
				r.nacked.Add(1)
			} else {
				r.unknown.Add(1)
			}
			attrs := []any{"row", row.ID, "exchange", row.Exchange, "key", row.RoutingKey, "outcome", out.String()}
			if perr != nil {
				attrs = append(attrs, "error", perr.Error())
			}
			r.log().Warn("relay publish failed; the batch stops and is retried", attrs...)
			r.release(ctx, fence, rest, r.backoff(row.Attempts))
			return false, errBroker
		}
	}
	return len(rows) >= r.batchSize(), nil
}

// mark records row as published. stop is true when the batch must end: the
// claim was lost (err nil) or the store failed (err set; the rows behind are
// handed back as far as possible).
func (r *Relay) mark(ctx context.Context, fence int64, row Row, routed bool, behind []Row) (stop bool, err error) {
	sctx, cancel := r.storeCtx(ctx)
	defer cancel()
	err = r.Store.MarkPublished(sctx, r.Owner, fence, []string{row.ID}, routed)
	switch {
	case err == nil:
		r.published.Add(1)
		return false, nil
	case errors.Is(err, ErrLeaseLost):
		r.leaseLost.Add(1)
		r.log().Warn("relay lost its lease; the batch stops", "row", row.ID)
		return true, nil
	default:
		r.storeErrors.Add(1)
		r.release(ctx, fence, behind, 0)
		return true, fmt.Errorf("mark outbox row published: %w", err)
	}
}

// release hands rows back to the outbox. A lost claim is counted; other
// failures only delay the rows until their lease expires.
func (r *Relay) release(ctx context.Context, fence int64, rows []Row, delay time.Duration) {
	if len(rows) == 0 {
		return
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	sctx, cancel := r.storeCtx(ctx)
	defer cancel()
	err := r.Store.Release(sctx, r.Owner, fence, ids, delay)
	switch {
	case err == nil:
	case errors.Is(err, ErrLeaseLost):
		r.leaseLost.Add(1)
	default:
		r.storeErrors.Add(1)
		r.log().Warn("relay could not release rows; they return when the lease expires", "rows", len(ids), "error", err.Error())
	}
}
