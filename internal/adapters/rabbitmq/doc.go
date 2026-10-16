// Source: umir-factory reference/durable@26166c8ba197

// Package rabbitmq is the reference RabbitMQ adapter of the durable stack:
// connector with reconnect, topology data and its declaration, confirmed
// publisher and consumer with a retry chain. The topology and the message
// properties follow docs/durable/events.md of the factory repository.
//
// Services copy this package to internal/adapters/rabbitmq; logic changes are
// made in the reference, a copy only changes import paths.
//
// Who may call what (events.md, "Кто объявляет топологию"):
//
//   - Declare runs only in the mq-init job.
//   - DeclareExclusive declares a service's own fan-out queues on every
//     (re)connection.
//   - CheckPassive verifies that the durable topology exists; it cannot compare
//     queue arguments (see its documentation).
package rabbitmq
