// Source: umir-factory reference/durable@26166c8ba197

package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// errShutdown tells the consume loop that ctx ended while a message was in hand.
var errShutdown = errors.New("rabbitmq: consumer is shutting down")

const (
	copyPublishTimeout    = 10 * time.Second
	defaultCopyPause      = 2 * time.Second
	classAttemptsExceeded = "attempts_exceeded"
	// attemptHeader counts the failed Handler attempts of a message. Only the
	// consumer sets it, on the copy it publishes to <q>.retry and <q>.dlq.
	attemptHeader = "x-umir-attempt"
	// failedAttemptsHeader carries the failure count on a .dlq copy; it is not
	// read back, so a message returned from .dlq counts from zero.
	failedAttemptsHeader = "x-umir-failed-attempts"
)

// consumerSeq makes consumer tags unique within the process.
var consumerSeq atomic.Uint64

// Delivery is one message handed to a Handler.
type Delivery struct {
	Queue      string
	Exchange   string
	RoutingKey string
	// MessageID and Type are the AMQP message_id and type properties.
	MessageID string
	Type      string
	// Headers are the message headers (x-umir-*, x-delivery-count and others).
	Headers map[string]any
	// Attempt is the 1-based number of this Handler attempt: 1 plus the value of
	// the header x-umir-attempt, which counts the failures the consumer has
	// recorded. Deliveries repeated after a crash without ack do not change it;
	// the broker limits those with x-delivery-limit. Always 1 on fan-out queues.
	Attempt int
	// Redelivered is the broker's flag: the message was delivered before and
	// not acknowledged.
	Redelivered bool
	Body        []byte
}

// Handler processes one delivery. A nil result acknowledges the message; an
// error sends it to the retry chain (or drops it on a fan-out queue). The
// Handler must be idempotent: a message can be delivered again after a crash.
// An error may implement Class() string to name its class in logs; the error
// text is never logged because it may quote the body.
type Handler func(ctx context.Context, d Delivery) error

type consumeConfig struct {
	setup       func(*amqp.Channel) error
	maxAttempts int
	fanout      bool
	copyPause   time.Duration
	logger      *slog.Logger
}

// ConsumeOption configures Consume.
type ConsumeOption func(*consumeConfig)

// WithSetup runs fn on the consuming channel of every new connection before
// consumption starts: DeclareExclusive for a fan-out queue, CheckPassive for a
// durable one.
func WithSetup(fn func(*amqp.Channel) error) ConsumeOption {
	return func(c *consumeConfig) { c.setup = fn }
}

// WithMaxAttempts sets the attempt after which a failing message goes to
// <queue>.dlq instead of the retry queue. The default is DefaultDeliveryLimit,
// the x-delivery-limit of the topology.
func WithMaxAttempts(n int) ConsumeOption {
	return func(c *consumeConfig) { c.maxAttempts = n }
}

// WithFanout marks the queue as an exclusive fan-out queue: no .retry and no
// .dlq, so a Handler error rejects the message without requeue and drops it.
func WithFanout() ConsumeOption {
	return func(c *consumeConfig) { c.fanout = true }
}

// WithCopyPause sets how long Consume waits before it publishes the copy of a
// failed message again when the broker did not confirm it (the default is 2 s).
func WithCopyPause(d time.Duration) ConsumeOption {
	return func(c *consumeConfig) { c.copyPause = d }
}

// WithLogger sets the logger of Consume (Connector.Consume uses the connector's).
func WithLogger(l *slog.Logger) ConsumeOption {
	return func(c *consumeConfig) { c.logger = l }
}

func newConsumeConfig(prefetch int, opts []ConsumeOption, fallback *slog.Logger) (consumeConfig, error) {
	cfg := consumeConfig{maxAttempts: DefaultDeliveryLimit, copyPause: defaultCopyPause, logger: fallback}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.logger == nil {
		cfg.logger = slog.New(slog.DiscardHandler)
	}
	switch {
	case prefetch < 1:
		// Zero means "unlimited" to the broker: the whole queue would sit unacknowledged.
		return cfg, errors.New("rabbitmq: prefetch must be positive")
	case cfg.maxAttempts < 1:
		return cfg, errors.New("rabbitmq: max attempts must be positive")
	case cfg.copyPause <= 0:
		return cfg, errors.New("rabbitmq: copy pause must be positive")
	}
	return cfg, nil
}

// Consume reads queue through the connector, reconnecting with backoff, until
// ctx ends; it returns ctx.Err() (or a configuration error at once). See the
// package-level Consume for the processing rules.
func (c *Connector) Consume(ctx context.Context, queue string, prefetch int, h Handler, opts ...ConsumeOption) error {
	cfg, err := newConsumeConfig(prefetch, opts, c.Logger)
	if err != nil {
		return err
	}
	return c.Run(ctx, func(ctx context.Context, conn *amqp.Connection, up func()) error {
		return consume(ctx, conn, queue, prefetch, h, cfg, up)
	})
}

// Consume reads queue on conn until ctx ends (nil) or the channel fails (error),
// handling one delivery at a time: the Handler is sequential, so prefetch is
// only the number of messages the broker keeps ready on this consumer. For
// task queues use 1: every prefetched message waits unacknowledged behind a
// slow Handler and counts as delivered if the process dies. Fan-out queues use
// FanoutPrefetch. prefetch must be positive.
//
//   - A Handler success acknowledges the message after the Handler returned.
//   - A Handler error on a work queue moves the message by the consumer's own
//     publish: it sends a copy to umir.dlx with the key <queue>.retry (after the
//     pause the broker returns it to <queue>), waits for the broker's confirm
//     and only then acknowledges the original. The copy carries x-umir-attempt,
//     the number of failures so far (on the .dlq copy x-umir-failed-attempts
//     instead, so that putting the message back resets the count).
//   - When attempt number maxAttempts fails, the copy goes to <queue>.dlq in
//     the same way.
//   - If the broker does not confirm the copy (no binding, a full queue), the
//     original stays unacknowledged and Consume publishes the copy again after
//     the copy pause, until it is confirmed, ctx ends or the channel fails.
//     Nothing is acknowledged before the copy is stored.
//   - A message whose recorded failures already reach maxAttempts before the
//     Handler runs goes to <queue>.dlq at once, without calling the Handler.
//   - A consumer that dies without ack is not counted here: the broker returns
//     the message and, after x-delivery-limit deliveries, dead-letters it to
//     <queue>.dlq itself (without x-umir-* metadata, with x-death).
//   - Logs and the copy's metadata carry message_id, type, attempt, queue and
//     the error class, never the body or the error text.
//
// If ctx ends while a Handler runs, its error is not counted as a failed
// attempt: the message stays unacknowledged and returns to the queue.
func Consume(ctx context.Context, conn *amqp.Connection, queue string, prefetch int, h Handler, opts ...ConsumeOption) error {
	cfg, err := newConsumeConfig(prefetch, opts, nil)
	if err != nil {
		return err
	}
	return consume(ctx, conn, queue, prefetch, h, cfg, func() {})
}

func consume(ctx context.Context, conn *amqp.Connection, queue string, prefetch int, h Handler, cfg consumeConfig, up func()) error {
	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("open channel: %w", err)
	}
	// Closing the channels gives unacknowledged messages back to the queue; the
	// conn belongs to the caller and may outlive this call.
	defer func() { ignoreErr(ch.Close()) }()
	closed := ch.NotifyClose(make(chan *amqp.Error, 1))
	if err := ch.Qos(prefetch, 0, false); err != nil {
		return fmt.Errorf("set prefetch: %w", err)
	}
	if cfg.setup != nil {
		if err := cfg.setup(ch); err != nil {
			return fmt.Errorf("setup: %w", err)
		}
	}
	var dlq *confirmChannel
	var dlqClosed <-chan *amqp.Error // nil on a fan-out queue: never ready
	if !cfg.fanout {
		if dlq, err = newConfirmChannel(conn); err != nil {
			return fmt.Errorf("dead-letter channel: %w", err)
		}
		defer dlq.close()
		dlqClosed = dlq.closed
	}
	tag := fmt.Sprintf("umir-%s-%d", queue, consumerSeq.Add(1))
	deliveries, err := ch.Consume(queue, tag, false, false, false, false, nil)
	if err != nil {
		return classify("consume "+queue, err)
	}
	defer func() { ignoreErr(ch.Cancel(tag, false)) }()
	up()
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-closed:
			if e == nil {
				return errors.New("channel closed")
			}
			return fmt.Errorf("channel closed: %w", e)
		case e := <-dlqClosed:
			// The channel that publishes copies is gone: reconnect, the unacknowledged message comes back.
			if e == nil {
				return errors.New("dead-letter channel closed")
			}
			return fmt.Errorf("dead-letter channel closed: %w", e)
		case raw, ok := <-deliveries:
			if !ok {
				return errors.New("deliveries closed")
			}
			if err := settle(ctx, raw, queue, h, cfg, dlq); err != nil {
				if errors.Is(err, errShutdown) {
					return nil
				}
				return err
			}
		}
	}
}

func toDelivery(raw amqp.Delivery, queue string, fanout bool) Delivery {
	d := Delivery{
		Queue: queue, Exchange: raw.Exchange, RoutingKey: raw.RoutingKey,
		MessageID: raw.MessageId, Type: raw.Type, Headers: raw.Headers,
		Attempt: 1, Redelivered: raw.Redelivered, Body: raw.Body,
	}
	if !fanout {
		d.Attempt = attemptFromHeaders(raw.Headers)
	}
	return d
}

// settle runs the Handler for one delivery and acknowledges, retries or dead-letters it.
func settle(ctx context.Context, raw amqp.Delivery, queue string, h Handler, cfg consumeConfig, dlq *confirmChannel) error {
	d := toDelivery(raw, queue, cfg.fanout)
	if !cfg.fanout && d.Attempt > cfg.maxAttempts {
		// The Handler already failed maxAttempts times: do not run it again.
		log := cfg.logger.With("queue", queue, "message_id", d.MessageID, "type", d.Type, "attempt", d.Attempt-1, "error_class", classAttemptsExceeded)
		return move(ctx, raw, d, d.Attempt-1, ".dlq", classAttemptsExceeded, cfg, dlq, log)
	}
	herr := h(ctx, d)
	if herr == nil {
		if err := raw.Ack(false); err != nil {
			return fmt.Errorf("ack: %w", err)
		}
		return nil
	}
	if ctx.Err() != nil {
		return errShutdown // leave the message unacknowledged, it comes back
	}
	class := errorClass(herr)
	log := cfg.logger.With("queue", queue, "message_id", d.MessageID, "type", d.Type, "attempt", d.Attempt, "error_class", class)
	if cfg.fanout {
		log.Warn("rabbitmq message rejected")
		if err := raw.Reject(false); err != nil {
			return fmt.Errorf("reject: %w", err)
		}
		return nil
	}
	suffix := ".retry"
	if d.Attempt >= cfg.maxAttempts {
		suffix = ".dlq"
	}
	return move(ctx, raw, d, d.Attempt, suffix, class, cfg, dlq, log)
}

// move publishes the copy of a failed message to umir.dlx with the key
// <queue><suffix> (".retry" or ".dlq"), waits until the broker confirms it as
// routed and only then acknowledges the original. failed is the number of
// failed attempts the copy records. An unconfirmed copy is published again
// after the copy pause with the original still unacknowledged; an unknown
// outcome ends the session instead, because the channel can no longer be
// trusted: the original comes back and the Handler runs again (at least once).
func move(ctx context.Context, raw amqp.Delivery, d Delivery, failed int, suffix, class string, cfg consumeConfig, dlq *confirmChannel, log *slog.Logger) error {
	key := d.Queue + suffix
	pub := buildCopy(raw, d.Queue, failed, suffix, class)
	for {
		pctx, cancel := context.WithTimeout(ctx, copyPublishTimeout)
		out, err := dlq.publish(pctx, ExchangeDLX, key, pub)
		cancel()
		switch out {
		case Confirmed:
			if suffix == ".dlq" {
				log.Error("rabbitmq message moved to the dead-letter queue")
			} else {
				log.Warn("rabbitmq message failed; retry follows")
			}
			if err := raw.Ack(false); err != nil {
				return fmt.Errorf("ack: %w", err)
			}
			return nil
		case Unknown:
			if ctx.Err() != nil {
				return errShutdown
			}
			return fmt.Errorf("copy to %s: %w", key, err)
		}
		log.Error("rabbitmq copy of a failed message was not confirmed; the original stays unacknowledged", "outcome", out.String(), "target", key)
		timer := time.NewTimer(cfg.copyPause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errShutdown
		case <-dlq.closed:
			timer.Stop()
			return fmt.Errorf("copy to %s: %w", key, ErrChannelClosed)
		case <-timer.C:
		}
	}
}

// buildCopy builds the message published for a failed delivery: the original
// identity, body and x-umir-* headers, the failure count and, for the dead-letter
// queue, the error class and the source queue.
func buildCopy(raw amqp.Delivery, queue string, failed int, suffix, class string) amqp.Publishing {
	headers := amqp.Table{}
	for k, v := range raw.Headers {
		if strings.HasPrefix(k, "x-umir-") {
			headers[k] = v
		}
	}
	if suffix == ".dlq" {
		// The dead-letter copy records the failures under another name, so a
		// message put back from .dlq by an operator starts again at attempt 1.
		delete(headers, attemptHeader)
		headers[failedAttemptsHeader] = int32(failed)
		headers["x-umir-error-class"] = class
		headers["x-umir-source-queue"] = queue
	} else {
		headers[attemptHeader] = int32(failed)
		delete(headers, failedAttemptsHeader)
		delete(headers, "x-umir-error-class")
		delete(headers, "x-umir-source-queue")
	}
	return amqp.Publishing{
		Headers: headers, ContentType: raw.ContentType, DeliveryMode: amqp.Persistent,
		MessageId: raw.MessageId, Type: raw.Type, Body: raw.Body,
	}
}

// attemptFromHeaders returns the number of the current Handler attempt: 1 plus
// the failures recorded in the header x-umir-attempt. A missing, malformed or
// negative header counts as no failures; a huge one is capped.
func attemptFromHeaders(headers amqp.Table) int {
	n := toInt64(headers[attemptHeader])
	n = max(0, min(n, 1<<20))
	return int(n) + 1
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	case int16:
		return int64(n)
	case uint8:
		return int64(n)
	case uint16:
		return int64(n)
	case uint32:
		return int64(n)
	case uint64:
		return int64(n)
	default:
		return 0
	}
}

// errorClass names the class of an error for logs and DLQ metadata without its
// text, which may quote the message body: Class() when the error offers it, a
// fixed name for context errors, the innermost Go type otherwise.
func errorClass(err error) string {
	var classed interface{ Class() string }
	if errors.As(err, &classed) {
		return classed.Class()
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	for {
		next := errors.Unwrap(err)
		if next == nil {
			break
		}
		err = next
	}
	return fmt.Sprintf("%T", err)
}
