// Source: umir-factory reference/durable@26166c8ba197

package relay

import "strings"

// ReturnPolicy says what the relay does with a row whose mandatory publish the
// broker returned because no queue was bound (docs/durable/events.md, column
// "return").
type ReturnPolicy int

const (
	// Retry keeps the row: it returns to the outbox with a delay and is
	// published again. It is a configuration error that an operator fixes by
	// declaring the binding (umir.tasks). Retry is the zero value, so an
	// unconfigured key never loses a message.
	Retry ReturnPolicy = iota
	// Accept marks the row published with routed=false and counts it in
	// Stats.ReturnedAccepted. The message is only a hint nobody needs to see
	// (umir.events, umir.auth): consumers reconcile from their source.
	Accept
)

func (p ReturnPolicy) String() string {
	if p == Accept {
		return "accept"
	}
	return "retry"
}

// PolicyFunc chooses the ReturnPolicy for an exchange and routing key.
// PolicyTable.PolicyFor has this shape.
type PolicyFunc func(exchange, routingKey string) ReturnPolicy

// PolicyRule applies Policy to messages of Exchange whose routing key matches
// Pattern.
type PolicyRule struct {
	// Exchange is the exchange name; empty matches every exchange.
	Exchange string
	// Pattern is an AMQP topic pattern: words separated by dots, "*" for exactly
	// one word, "#" for zero or more words. A pattern without wildcards matches
	// the key exactly.
	Pattern string
	Policy  ReturnPolicy
}

// PolicyTable chooses a ReturnPolicy from ordered rules. The first matching
// rule wins; Default applies when none matches.
type PolicyTable struct {
	Default ReturnPolicy
	Rules   []PolicyRule
}

// DefaultPolicies is the table of docs/durable/events.md: accept for umir.events
// and umir.auth, retry for umir.tasks and everything unknown.
func DefaultPolicies() PolicyTable {
	return PolicyTable{
		Default: Retry,
		Rules: []PolicyRule{
			{Exchange: "umir.events", Pattern: "#", Policy: Accept},
			{Exchange: "umir.auth", Pattern: "#", Policy: Accept},
			{Exchange: "umir.tasks", Pattern: "#", Policy: Retry},
		},
	}
}

// PolicyFor returns the policy of the first rule that matches.
func (t PolicyTable) PolicyFor(exchange, routingKey string) ReturnPolicy {
	for _, r := range t.Rules {
		if (r.Exchange == "" || r.Exchange == exchange) && topicMatch(r.Pattern, routingKey) {
			return r.Policy
		}
	}
	return t.Default
}

// topicMatch reports whether key matches the AMQP topic pattern.
func topicMatch(pattern, key string) bool {
	return matchWords(strings.Split(pattern, "."), strings.Split(key, "."))
}

func matchWords(pattern, key []string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case "#":
			rest := pattern[1:]
			if len(rest) == 0 {
				return true
			}
			for i := 0; i <= len(key); i++ {
				if matchWords(rest, key[i:]) {
					return true
				}
			}
			return false
		case "*":
			if len(key) == 0 {
				return false
			}
		default:
			if len(key) == 0 || key[0] != pattern[0] {
				return false
			}
		}
		pattern, key = pattern[1:], key[1:]
	}
	return len(key) == 0
}
