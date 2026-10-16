// Source: umir-factory reference/durable@26166c8ba197

package rabbitmq

import (
	"errors"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Topology errors. A broker answer is classified by its AMQP reply code; the
// original error stays wrapped.
var (
	// ErrTopologyMissing: an exchange or queue does not exist (404).
	ErrTopologyMissing = errors.New("rabbitmq: topology element is missing")
	// ErrTopologyMismatch: an exchange or queue exists with other arguments (406).
	ErrTopologyMismatch = errors.New("rabbitmq: topology element exists with other arguments")
	// ErrAccessRefused: the account may not do this on the broker (403).
	ErrAccessRefused = errors.New("rabbitmq: access refused")
)

// classify attaches the matching sentinel to an AMQP channel exception.
func classify(what string, err error) error {
	var ae *amqp.Error
	if errors.As(err, &ae) {
		switch ae.Code {
		case amqp.NotFound:
			return fmt.Errorf("%s: %w: %w", what, ErrTopologyMissing, err)
		case amqp.PreconditionFailed:
			return fmt.Errorf("%s: %w: %w", what, ErrTopologyMismatch, err)
		case amqp.AccessRefused:
			return fmt.Errorf("%s: %w: %w", what, ErrAccessRefused, err)
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

// Declare declares the durable topology: exchanges, quorum work queues with
// their .retry and .dlq queues and all bindings. It is idempotent: declaring
// the same topology again changes nothing. A queue or exchange that exists with
// other arguments is an error (ErrTopologyMismatch), never a deletion.
//
// Only the mq-init job calls Declare. A failure closes ch (a broker channel
// exception): open a new channel afterwards.
func Declare(ch *amqp.Channel, t Topology) error {
	if err := t.Validate(); err != nil {
		return err
	}
	for _, e := range t.Exchanges {
		if err := ch.ExchangeDeclare(e.Name, e.Kind, true, false, false, false, nil); err != nil {
			return classify("declare exchange "+e.Name, err)
		}
	}
	for _, q := range t.Queues() {
		if _, err := ch.QueueDeclare(q.Name, true, false, false, false, q.Args); err != nil {
			return classify("declare queue "+q.Name, err)
		}
	}
	for _, b := range t.Bindings() {
		if err := ch.QueueBind(b.Queue, b.Key, b.Exchange, false, nil); err != nil {
			return classify(fmt.Sprintf("bind %s to %s", b.Queue, b.Exchange), err)
		}
	}
	return nil
}

// DeclareExclusive declares a replica's own fan-out queue (exclusive,
// auto-delete, not durable) and its bindings. A service calls it on every
// (re)connection: the queue vanishes with the connection. It needs the
// exchanges to exist.
func DeclareExclusive(ch *amqp.Channel, q ExclusiveQueue) error {
	if _, err := ch.QueueDeclare(q.Name, false, true, true, false, nil); err != nil {
		return classify("declare queue "+q.Name, err)
	}
	for _, b := range q.Bindings {
		if err := ch.QueueBind(q.Name, b.Key, b.Exchange, false, nil); err != nil {
			return classify(fmt.Sprintf("bind %s to %s", q.Name, b.Exchange), err)
		}
	}
	return nil
}

// CheckPassive verifies with passive declarations that every exchange and queue
// of the topology exists (ErrTopologyMissing otherwise). Passive declaration
// needs no configure permission, so every service can use it at start and on
// reconnection.
//
// Limit: AMQP 0-9-1 ignores arguments and types in a passive declaration, so
// CheckPassive cannot see a queue with other arguments. Only Declare (the
// idempotent mq-init job) finds that mismatch; run it on every deployment. A
// failure closes ch: open a new channel afterwards.
func CheckPassive(ch *amqp.Channel, t Topology) error {
	if err := t.Validate(); err != nil {
		return err
	}
	for _, e := range t.Exchanges {
		if err := ch.ExchangeDeclarePassive(e.Name, e.Kind, true, false, false, false, nil); err != nil {
			return classify("check exchange "+e.Name, err)
		}
	}
	for _, q := range t.Queues() {
		if _, err := ch.QueueDeclarePassive(q.Name, true, false, false, false, nil); err != nil {
			return classify("check queue "+q.Name, err)
		}
	}
	return nil
}
