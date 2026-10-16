// Source: umir-factory reference/durable@26166c8ba197

// Package relay moves outbox rows to the broker: it claims a batch of rows
// under a lease, publishes them in position order and marks a row published
// only after the broker confirmed it.
//
// The package depends only on the interfaces it defines (OutboxStore and
// Publisher) and imports no other package of the reference module, so a
// service copies it next to its postgres and rabbitmq adapters unchanged. The
// service wires the adapters in its composition root. Both adapters are thin:
//
//   - OutboxStore: postgres.Outbox has the same methods; convert []postgres.Row
//     to []Row field by field (the fields have the same names and types),
//     convert postgres.OutboxStats to Backlog (same fields), and wrap
//     postgres.ErrLeaseLost so that errors.Is(err, relay.ErrLeaseLost) holds.
//   - Publisher: rabbitmq.Publisher.Publish takes a rabbitmq.Message with the
//     same fields as Message, so Message converts directly; Outcome converts
//     from rabbitmq.Outcome by integer conversion (the values are the same).
//
// # Delivery rules
//
//   - The message is built from the row. message_id is the eventID of the body
//     of an event (docs/durable/events.md: message_id = eventID), or the
//     messageID of a task body; type is the routing key. Schema and generation
//     come from the headers x-umir-schema and x-umir-generation and, when a
//     header is missing, from schemaVersion and generation of the body. A row
//     that yields no usable message is invalid: it is held back for RetryMax
//     and counted in Stats.Invalid, and the rows behind it still go out.
//   - A row is marked only after Confirmed. Unknown repeats the row later with
//     the same message_id, because the message is rebuilt from the same row; the
//     consumer deduplicates by (consumer, eventID).
//   - Returned (mandatory return) follows the ReturnPolicy of the exchange and
//     routing key: Accept marks the row published with routed=false and counts
//     it in Stats.ReturnedAccepted; Retry returns the row to the queue with a
//     delay, and the relay carries on with the next row.
//   - Nacked and Unknown are broker or connection failures: the relay stops the
//     batch, returns the failed row and every row behind it with a backoff that
//     grows with the attempt number, and waits before the next claim with a pause
//     that doubles from the poll interval up to 5 s (a confirmed batch resets it). Nothing is published
//     behind a failed row, so a later event of an aggregate never overtakes it
//     within a batch.
//   - A lost lease (ErrLeaseLost) stops the batch at once: the rows belong to
//     another relay now.
//   - Run returns ctx.Err() after handing the unpublished rows of its batch
//     back, using a context that survives the cancellation for a short time.
package relay
