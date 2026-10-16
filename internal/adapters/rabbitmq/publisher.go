// Source: umir-factory reference/durable@26166c8ba197

package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"
)

var (
	// ErrNotConnected: there is no usable broker session now; nothing was sent.
	ErrNotConnected = errors.New("rabbitmq: publisher is not connected")
	// ErrInvalidMessage: the message lacks a field the contract requires; repeating it cannot help.
	ErrInvalidMessage = errors.New("rabbitmq: invalid message")
)

// Message is one message to publish, with the properties of docs/durable/events.md.
type Message struct {
	Exchange   string
	RoutingKey string
	// MessageID is the AMQP message_id: eventID of an event, messageID of a task.
	MessageID string
	// Type is the AMQP type: <owner>.<eventType>, or the routing key for a task.
	Type string
	// Schema is the header x-umir-schema (schemaVersion), at least 1.
	Schema int
	// RestaurantID is the header x-umir-restaurant-id; no header when empty.
	RestaurantID string
	// Generation is the header x-umir-generation.
	Generation string
	// Body is the JSON body. content_type is application/json and delivery_mode 2.
	Body []byte
}

func (m Message) validate() error {
	switch {
	case m.Exchange == "":
		return fmt.Errorf("%w: exchange is empty", ErrInvalidMessage)
	case m.RoutingKey == "":
		return fmt.Errorf("%w: routing key is empty", ErrInvalidMessage)
	case m.MessageID == "":
		return fmt.Errorf("%w: message ID is empty", ErrInvalidMessage)
	case m.Type == "":
		return fmt.Errorf("%w: type is empty", ErrInvalidMessage)
	case m.Schema < 1:
		return fmt.Errorf("%w: schema version must be at least 1", ErrInvalidMessage)
	case m.Generation == "":
		return fmt.Errorf("%w: generation is empty", ErrInvalidMessage)
	}
	return nil
}

func (m Message) publishing() amqp.Publishing {
	headers := amqp.Table{
		"x-umir-schema":     int32(m.Schema),
		"x-umir-generation": m.Generation,
	}
	if m.RestaurantID != "" {
		headers["x-umir-restaurant-id"] = m.RestaurantID
	}
	return amqp.Publishing{
		Headers:      headers,
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    m.MessageID,
		Type:         m.Type,
		Body:         m.Body,
	}
}

// Publisher publishes messages with the mandatory flag and publisher confirms
// over a connection that a Connector keeps alive. Run it in a goroutine of the
// owner; Publish is safe for concurrent use and publishes one message at a time.
type Publisher struct {
	connector *Connector
	setup     func(*amqp.Channel) error

	sem chan struct{} // one publish at a time

	mu  sync.RWMutex
	cur *pubSession
}

// pubSession is one confirm channel of one connection.
type pubSession struct {
	cc     *confirmChannel
	retire chan struct{} // closed to end the session after an unknown outcome
	once   sync.Once
}

func (s *pubSession) end() { s.once.Do(func() { close(s.retire) }) }

// PublisherOption configures a Publisher.
type PublisherOption func(*Publisher)

// WithPublisherSetup runs fn on the publisher's channel of every new connection
// before it is reported ready, for example CheckPassive or, in mq-init,
// Declare. An error ends the session and the connector reconnects with
// backoff.
func WithPublisherSetup(fn func(*amqp.Channel) error) PublisherOption {
	return func(p *Publisher) { p.setup = fn }
}

// NewPublisher creates a publisher on its own connection of c.
func NewPublisher(c *Connector, opts ...PublisherOption) *Publisher {
	p := &Publisher{connector: c, sem: make(chan struct{}, 1)}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Run keeps the publisher's connection alive until ctx ends and returns ctx.Err().
func (p *Publisher) Run(ctx context.Context) error {
	return p.connector.Run(ctx, p.session)
}

// Ready reports whether a session is up: a Publish now has a channel to use.
func (p *Publisher) Ready() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cur != nil
}

func (p *Publisher) session(ctx context.Context, conn *amqp.Connection, up func()) error {
	cc, err := newConfirmChannel(conn)
	if err != nil {
		return err
	}
	defer cc.close()
	closed := cc.closed
	if p.setup != nil {
		if err := p.setup(cc.ch); err != nil {
			return fmt.Errorf("setup: %w", err)
		}
	}
	s := &pubSession{cc: cc, retire: make(chan struct{})}
	p.mu.Lock()
	p.cur = s
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.cur == s {
			p.cur = nil
		}
		p.mu.Unlock()
	}()
	up()
	select {
	case <-ctx.Done():
		return nil
	case e := <-closed:
		if e == nil {
			return errors.New("channel closed")
		}
		return fmt.Errorf("channel closed: %w", e)
	case <-s.retire:
		return errors.New("session retired after an unknown publish outcome")
	}
}

// Publish sends msg and waits for the broker's verdict or ctx. The error is
// non-nil exactly when the outcome is Unknown. The result is Confirmed only after
// a positive publisher confirm for a message the broker routed.
//
// After an Unknown outcome caused by ctx the session is retired and rebuilt, so
// a late confirm or return of the abandoned message cannot be mistaken for the
// next one.
func (p *Publisher) Publish(ctx context.Context, msg Message) (Outcome, error) {
	if err := msg.validate(); err != nil {
		return Unknown, err
	}
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return Unknown, fmt.Errorf("wait for the publisher: %w", ctx.Err())
	}
	p.mu.RLock()
	s := p.cur
	p.mu.RUnlock()
	if s == nil {
		return Unknown, ErrNotConnected
	}
	out, err := s.cc.publish(ctx, msg.Exchange, msg.RoutingKey, msg.publishing())
	if out == Unknown {
		s.end()
		p.mu.Lock()
		if p.cur == s {
			p.cur = nil
		}
		p.mu.Unlock()
	}
	return out, err
}
