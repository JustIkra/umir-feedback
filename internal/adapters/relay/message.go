// Source: umir-factory reference/durable@26166c8ba197

package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Message headers of docs/durable/events.md carried in Row.Headers.
const (
	headerSchema       = "x-umir-schema"
	headerGeneration   = "x-umir-generation"
	headerRestaurantID = "x-umir-restaurant-id"
)

// ErrInvalidRow: the row cannot become a message of the catalog (no eventID or
// messageID, no schema version or generation, body that is not a JSON object).
// Repeating it cannot help; the relay holds such a row back and counts it.
var ErrInvalidRow = errors.New("relay: outbox row is not a valid message")

// bodyFields are the envelope fields the relay reads from the body.
type bodyFields struct {
	EventID       string `json:"eventID"`
	MessageID     string `json:"messageID"`
	SchemaVersion int    `json:"schemaVersion"`
	Generation    string `json:"generation"`
}

// buildMessage derives the broker message from a row: message_id is the eventID
// of an event body or the messageID of a task body, type is the routing key,
// schema and generation come from the headers and, failing that, from the body.
// The result depends only on the row, so a repeat carries the same message_id.
func buildMessage(row Row) (Message, error) {
	var body bodyFields
	if err := json.Unmarshal(row.Body, &body); err != nil {
		return Message{}, fmt.Errorf("%w: body is not a JSON object", ErrInvalidRow)
	}
	id := body.EventID
	if id == "" {
		id = body.MessageID
	}
	if id == "" {
		return Message{}, fmt.Errorf("%w: body has no eventID or messageID", ErrInvalidRow)
	}
	schema := body.SchemaVersion
	if h := row.Headers[headerSchema]; h != "" {
		n, err := strconv.Atoi(h)
		if err != nil {
			return Message{}, fmt.Errorf("%w: %s is not an integer", ErrInvalidRow, headerSchema)
		}
		schema = n
	}
	if schema < 1 {
		return Message{}, fmt.Errorf("%w: no schema version", ErrInvalidRow)
	}
	generation := body.Generation
	if h := row.Headers[headerGeneration]; h != "" {
		generation = h
	}
	if generation == "" {
		return Message{}, fmt.Errorf("%w: no generation", ErrInvalidRow)
	}
	restaurant := row.Headers[headerRestaurantID]
	if restaurant == "" {
		restaurant = row.RestaurantID
	}
	return Message{
		Exchange:     row.Exchange,
		RoutingKey:   row.RoutingKey,
		MessageID:    id,
		Type:         row.RoutingKey,
		Schema:       schema,
		RestaurantID: restaurant,
		Generation:   generation,
		Body:         row.Body,
	}, nil
}
