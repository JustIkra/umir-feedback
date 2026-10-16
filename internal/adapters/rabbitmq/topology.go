// Source: umir-factory reference/durable@26166c8ba197

package rabbitmq

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Exchange names of docs/durable/events.md.
const (
	ExchangeEvents = "umir.events"
	ExchangeAuth   = "umir.auth"
	ExchangeTasks  = "umir.tasks"
	ExchangeDLX    = "umir.dlx"
)

// Work queue names of docs/durable/events.md.
const (
	QueueAuthMail         = "auth.mail"
	QueueFeedbackSMTP     = "feedback.smtp"
	QueueFeedbackTelegram = "feedback.telegram"
	QueueFeedbackAuth     = "feedback.auth"
)

// Defaults of the retry chain (events.md, "Очереди задач").
//
// The chain has two halves that never meet. A Handler failure is moved by the
// consumer itself: it publishes a copy to umir.dlx with the key <q>.retry (or
// <q>.dlq after the last attempt) and acknowledges the original only after the
// broker confirmed the copy. A consumer that dies without ack is handled by the
// broker: x-delivery-limit dead-letters the message straight to <q>.dlq. The
// work queue must not dead-letter into <q>.retry: <q>.retry returns messages to
// <q> and RabbitMQ drops a message that comes back to a queue it was
// dead-lettered from for a reason other than "rejected" (a cycle).
const (
	// DefaultRetryTTL is the pause in <q>.retry before a message returns to <q>.
	DefaultRetryTTL = 30 * time.Second
	// DefaultDeliveryLimit is x-delivery-limit of work queues, the number of
	// broker deliveries without ack after which the broker dead-letters the
	// message to <q>.dlq, and the number of Handler attempts before Consume does.
	DefaultDeliveryLimit = 5
	// FanoutPrefetch is the prefetch of the exclusive fan-out queues.
	FanoutPrefetch = 32
)

// AuthEventKeys are the routing keys of umir.auth, in the order of events.md.
var AuthEventKeys = []string{
	"auth.restaurant.deleting",
	"auth.restaurant.deleted",
	"auth.organization.deleting",
	"auth.organization.deleted",
	"auth.organization.settings_changed",
	"auth.membership.revoked",
	"auth.assignment.revoked",
}

// WarehouseEventKeys are the routing keys of umir.events, in the order of
// events.md. Consumers bind the wildcard WarehouseBinding instead.
var WarehouseEventKeys = []string{
	"warehouse.ProductRegistered",
	"warehouse.DishDefined",
	"warehouse.DeliveryReceived",
	"warehouse.OrderPlaced",
	"warehouse.BatchExpiryPending",
	"warehouse.BatchWriteoffConfirmed",
	"warehouse.BatchExpired",
}

// WarehouseBinding is the only binding Gateway makes on umir.events (C38).
const WarehouseBinding = "warehouse.#"

// ExchangeSpec is a durable exchange.
type ExchangeSpec struct {
	Name string
	Kind string // amqp.ExchangeTopic or amqp.ExchangeDirect
}

// BindingSpec binds a queue to an exchange with a routing key.
type BindingSpec struct {
	Queue    string
	Exchange string
	Key      string
}

// QueueSpec is a durable quorum queue with its arguments.
type QueueSpec struct {
	Name string
	Args amqp.Table
}

// WorkQueueSpec is a work queue <q> with its <q>.retry and <q>.dlq and the
// bindings of <q> to exchanges other than umir.dlx. The three bindings on
// umir.dlx are implied.
type WorkQueueSpec struct {
	Name string
	// Bindings lists the exchanges and keys that feed the queue; the Queue
	// field is filled in by Topology.Bindings.
	Bindings []BindingSpec
}

// Topology is the durable topology declared by mq-init: exchanges, work queues
// with their retry and dead-letter queues and all bindings. It is plain data;
// tests change RetryTTL to speed up the retry chain.
type Topology struct {
	Exchanges     []ExchangeSpec
	Work          []WorkQueueSpec
	RetryTTL      time.Duration
	DeliveryLimit int
}

// TopologyOptions selects optional parts of the topology.
type TopologyOptions struct {
	// Feedback adds feedback.auth (C45): only when the Feedback profile is on.
	Feedback bool
}

// DefaultTopology returns the topology of docs/durable/events.md.
func DefaultTopology(opts TopologyOptions) Topology {
	t := Topology{
		Exchanges: []ExchangeSpec{
			{Name: ExchangeEvents, Kind: amqp.ExchangeTopic},
			{Name: ExchangeAuth, Kind: amqp.ExchangeTopic},
			{Name: ExchangeTasks, Kind: amqp.ExchangeDirect},
			{Name: ExchangeDLX, Kind: amqp.ExchangeDirect},
		},
		RetryTTL:      DefaultRetryTTL,
		DeliveryLimit: DefaultDeliveryLimit,
	}
	for _, q := range []string{QueueAuthMail, QueueFeedbackSMTP, QueueFeedbackTelegram} {
		t.Work = append(t.Work, WorkQueueSpec{Name: q, Bindings: []BindingSpec{{Exchange: ExchangeTasks, Key: q}}})
	}
	if opts.Feedback {
		t.Work = append(t.Work, WorkQueueSpec{Name: QueueFeedbackAuth, Bindings: []BindingSpec{
			{Exchange: ExchangeAuth, Key: "auth.restaurant.deleted"},
			{Exchange: ExchangeAuth, Key: "auth.organization.deleted"},
		}})
	}
	return t
}

// Validate reports a topology that cannot be declared.
func (t Topology) Validate() error {
	if t.RetryTTL < time.Millisecond {
		return errors.New("rabbitmq topology: retry TTL must be at least 1ms")
	}
	if t.DeliveryLimit < 1 {
		return errors.New("rabbitmq topology: delivery limit must be positive")
	}
	exchanges := map[string]bool{}
	for _, e := range t.Exchanges {
		if e.Name == "" || (e.Kind != amqp.ExchangeTopic && e.Kind != amqp.ExchangeDirect) {
			return fmt.Errorf("rabbitmq topology: exchange %q has an invalid name or kind %q", e.Name, e.Kind)
		}
		exchanges[e.Name] = true
	}
	if !exchanges[ExchangeDLX] {
		return fmt.Errorf("rabbitmq topology: exchange %s is required for the retry chain", ExchangeDLX)
	}
	seen := map[string]bool{}
	for _, w := range t.Work {
		if w.Name == "" || seen[w.Name] {
			return fmt.Errorf("rabbitmq topology: work queue %q is empty or repeated", w.Name)
		}
		seen[w.Name] = true
		for _, b := range w.Bindings {
			if !exchanges[b.Exchange] || b.Key == "" {
				return fmt.Errorf("rabbitmq topology: binding of %s to exchange %q key %q is invalid", w.Name, b.Exchange, b.Key)
			}
		}
	}
	return nil
}

// Queues expands the work queues into the queues to declare, in declaration
// order: <q>.dlq, <q>.retry, <q> for each work queue (targets first).
func (t Topology) Queues() []QueueSpec {
	var out []QueueSpec
	for _, w := range t.Work {
		out = append(out,
			QueueSpec{Name: w.Name + ".dlq", Args: amqp.Table{"x-queue-type": "quorum"}},
			QueueSpec{Name: w.Name + ".retry", Args: amqp.Table{
				"x-queue-type":              "quorum",
				"x-message-ttl":             t.RetryTTL.Milliseconds(),
				"x-overflow":                "reject-publish",
				"x-dead-letter-exchange":    ExchangeDLX,
				"x-dead-letter-routing-key": w.Name,
				"x-dead-letter-strategy":    "at-least-once",
			}},
			QueueSpec{Name: w.Name, Args: amqp.Table{
				"x-queue-type":              "quorum",
				"x-delivery-limit":          int64(t.DeliveryLimit),
				"x-overflow":                "reject-publish",
				"x-dead-letter-exchange":    ExchangeDLX,
				"x-dead-letter-routing-key": w.Name + ".dlq",
				"x-dead-letter-strategy":    "at-least-once",
			}},
		)
	}
	return out
}

// Bindings expands the work queues into all bindings: the three umir.dlx
// bindings of each work queue and the bindings that feed it.
func (t Topology) Bindings() []BindingSpec {
	var out []BindingSpec
	for _, w := range t.Work {
		out = append(out,
			BindingSpec{Queue: w.Name, Exchange: ExchangeDLX, Key: w.Name},
			BindingSpec{Queue: w.Name + ".retry", Exchange: ExchangeDLX, Key: w.Name + ".retry"},
			BindingSpec{Queue: w.Name + ".dlq", Exchange: ExchangeDLX, Key: w.Name + ".dlq"},
		)
		for _, b := range w.Bindings {
			b.Queue = w.Name
			out = append(out, b)
		}
	}
	return out
}

// ExclusiveQueue is a fan-out queue of one replica: exclusive, auto-delete,
// not quorum, without retry or dead-letter queues.
type ExclusiveQueue struct {
	Name     string
	Bindings []BindingSpec
}

var instancePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func exclusiveQueue(prefix, instance, exchange string, keys []string) (ExclusiveQueue, error) {
	if !instancePattern.MatchString(instance) {
		return ExclusiveQueue{}, errors.New("rabbitmq: instance ID must match [A-Za-z0-9_-]+")
	}
	q := ExclusiveQueue{Name: prefix + instance}
	for _, key := range keys {
		q.Bindings = append(q.Bindings, BindingSpec{Queue: q.Name, Exchange: exchange, Key: key})
	}
	return q, nil
}

// GatewayLiveQueue is gateway.live.<instance>, bound to warehouse.# on umir.events.
func GatewayLiveQueue(instance string) (ExclusiveQueue, error) {
	return exclusiveQueue("gateway.live.", instance, ExchangeEvents, []string{WarehouseBinding})
}

// GatewayAuthQueue is gateway.auth.<instance>, bound to all seven auth keys.
func GatewayAuthQueue(instance string) (ExclusiveQueue, error) {
	return exclusiveQueue("gateway.auth.", instance, ExchangeAuth, AuthEventKeys)
}

// ServingAuthQueue is serving.auth.<instance>, bound to the five auth keys
// Serving reacts to.
func ServingAuthQueue(instance string) (ExclusiveQueue, error) {
	return exclusiveQueue("serving.auth.", instance, ExchangeAuth, []string{
		"auth.restaurant.deleting",
		"auth.restaurant.deleted",
		"auth.organization.deleting",
		"auth.organization.deleted",
		"auth.organization.settings_changed",
	})
}
