// Source: umir-factory reference/durable@26166c8ba197

package rabbitmq

import (
	"context"
	"errors"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Outcome is what is known about a published message.
type Outcome int

const (
	// Unknown: the result is not known (connection lost, timeout, nothing
	// sent). The message may or may not be on the broker; the caller repeats it
	// with the same message ID. It is the zero value, so an unset Outcome is
	// never mistaken for success. Publish returns a non-nil error with Unknown.
	Unknown Outcome = iota
	// Confirmed: the broker routed the message to at least one queue and
	// confirmed it positively (for quorum queues: replicated).
	Confirmed
	// Returned: the broker could not route the message (mandatory return, no
	// matching binding). The message is not stored anywhere.
	Returned
	// Nacked: the broker refused the message (for example a queue with
	// overflow=reject-publish is full). The message is not stored.
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

// ErrChannelClosed: the channel closed while a publish was waiting for its confirm.
var ErrChannelClosed = errors.New("rabbitmq: channel closed before the publisher confirm")

// returnBuffer bounds the returns the client library may queue before it
// blocks its reader; publishes are serial, so at most one is outstanding.
const returnBuffer = 16

// confirmChannel is a channel in publisher-confirm mode that publishes with
// the mandatory flag, one message at a time. It is not safe for concurrent use.
type confirmChannel struct {
	ch      *amqp.Channel
	returns chan amqp.Return
	// closed receives the channel exception, if any, and is then closed.
	closed <-chan *amqp.Error
}

// newConfirmChannel opens a channel on conn and switches it to confirm mode.
func newConfirmChannel(conn *amqp.Connection) (*confirmChannel, error) {
	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("open channel: %w", err)
	}
	if err := ch.Confirm(false); err != nil {
		ignoreErr(ch.Close())
		return nil, fmt.Errorf("enable publisher confirms: %w", err)
	}
	return &confirmChannel{
		ch:      ch,
		returns: ch.NotifyReturn(make(chan amqp.Return, returnBuffer)),
		closed:  ch.NotifyClose(make(chan *amqp.Error, 1)),
	}, nil
}

// close closes the channel; an already closed channel is not an error.
func (c *confirmChannel) close() {
	ignoreErr(c.ch.Close())
}

// publish sends one mandatory message and waits for the broker's verdict. The
// broker sends basic.return before basic.ack for the same message and the
// client library dispatches both in order, so after the confirm a return for
// this message is already queued.
func (c *confirmChannel) publish(ctx context.Context, exchange, key string, pub amqp.Publishing) (Outcome, error) {
	for drained := false; !drained; {
		select {
		case <-c.returns:
		default:
			drained = true
		}
	}
	dc, err := c.ch.PublishWithDeferredConfirmWithContext(ctx, exchange, key, true, false, pub)
	if err != nil {
		return Unknown, fmt.Errorf("publish: %w", err)
	}
	select {
	case <-dc.Done():
	case <-ctx.Done():
		return Unknown, fmt.Errorf("wait for publisher confirm: %w", ctx.Err())
	}
	if !dc.Acked() {
		// The client library nacks pending confirms when the channel closes.
		if c.ch.IsClosed() {
			return Unknown, ErrChannelClosed
		}
		return Nacked, nil
	}
	for {
		select {
		case r := <-c.returns:
			if r.MessageId == pub.MessageId {
				return Returned, nil
			}
		default:
			return Confirmed, nil
		}
	}
}

// ignoreErr documents a deliberately ignored error from a shutdown path, where
// the peer may already have closed the connection.
func ignoreErr(error) {}
