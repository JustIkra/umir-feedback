// Source: umir-factory reference/durable@26166c8ba197

package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	dialTimeout      = 5 * time.Second
	defaultHeartbeat = 10 * time.Second
	closeTimeout     = 3 * time.Second
	// maxBackoff caps the wait between two connection attempts.
	maxBackoff  = 30 * time.Second
	baseBackoff = 500 * time.Millisecond
	// defaultStableAfter is how long a session must stay up before the next
	// failure starts the backoff from the beginning: a connection that is
	// accepted and dropped at once must not defeat the backoff.
	defaultStableAfter = 10 * time.Second
)

// BrokerURL is the AMQP URL of a broker. It carries credentials, so it prints
// as a placeholder with every fmt verb, in logs and in %+v of a struct that
// holds it; Reveal returns the text for the dialer.
type BrokerURL string

// String implements fmt.Stringer.
func (BrokerURL) String() string { return "<amqp-url>" }

// GoString implements fmt.GoStringer, which %#v uses.
func (BrokerURL) GoString() string { return "rabbitmq.BrokerURL(<amqp-url>)" }

// LogValue implements slog.LogValuer.
func (BrokerURL) LogValue() slog.Value { return slog.StringValue("<amqp-url>") }

// MarshalText implements encoding.TextMarshaler, which encoding/json uses.
func (BrokerURL) MarshalText() ([]byte, error) { return []byte("<amqp-url>"), nil }

// Reveal returns the URL text, credentials included. Pass it only to a dialer.
func (u BrokerURL) Reveal() string { return string(u) }

// Backoff returns how long to wait before connection attempt number attempt (0 is the first retry).
type Backoff func(attempt int) time.Duration

// ExponentialBackoff doubles the wait from 500 ms up to 30 s.
func ExponentialBackoff(attempt int) time.Duration {
	if attempt < 0 || attempt > 6 {
		return maxBackoff
	}
	return min(baseBackoff<<attempt, maxBackoff)
}

// Dialer opens a broker connection with the given connection name. It must
// give up when ctx ends.
type Dialer func(ctx context.Context, url, name string) (*amqp.Connection, error)

// Connector keeps one broker connection alive: it dials, runs a session on it and, when the session ends
// or the connection drops, dials again after a backoff. It owns no goroutine of its own; Run blocks.
// The zero values of Dial, Backoff, Heartbeat and Logger select the defaults.
type Connector struct {
	URL BrokerURL
	// Name is the connection name shown in the management UI.
	Name string
	// Heartbeat is the AMQP heartbeat interval, 10 s when zero.
	Heartbeat time.Duration
	Dial      Dialer
	Backoff   Backoff
	// StableAfter is how long a session must stay up before a failure resets
	// the backoff, 10 s when zero.
	StableAfter time.Duration
	// Logger receives connection events; it never receives message bodies or the URL.
	Logger *slog.Logger

	now func() time.Time // test hook; time.Now when nil
}

// Session works on one connection. It calls up once the connection is usable, which resets the backoff,
// and returns when the session is over: nil when ctx ended, an error when the connection or channel failed.
type Session func(ctx context.Context, conn *amqp.Connection, up func()) error

func (c *Connector) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (c *Connector) dial() Dialer {
	if c.Dial != nil {
		return c.Dial
	}
	heartbeat := c.Heartbeat
	if heartbeat <= 0 {
		heartbeat = defaultHeartbeat
	}
	return func(ctx context.Context, rawURL, name string) (*amqp.Connection, error) {
		dialer := net.Dialer{Timeout: dialTimeout}
		// The AMQP handshake runs on the connection after the TCP dial: close
		// the socket when ctx ends so that a silent broker cannot hold Run.
		var release func() bool
		conn, err := amqp.DialConfig(rawURL, amqp.Config{
			Heartbeat: heartbeat,
			Dial: func(network, addr string) (net.Conn, error) {
				nc, err := dialer.DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				if err := nc.SetDeadline(time.Now().Add(dialTimeout)); err != nil {
					ignoreErr(nc.Close())
					return nil, err
				}
				release = context.AfterFunc(ctx, func() { ignoreErr(nc.Close()) })
				return nc, nil
			},
			Properties: amqp.Table{"connection_name": name},
		})
		if release != nil {
			release()
		}
		return conn, err
	}
}

func (c *Connector) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *Connector) stableAfter() time.Duration {
	if c.StableAfter > 0 {
		return c.StableAfter
	}
	return defaultStableAfter
}

// Run connects and runs sessions until ctx ends. It never returns an error other than ctx.Err().
func (c *Connector) Run(ctx context.Context, session Session) error {
	return c.loop(ctx, func(up func()) error { return c.once(ctx, session, up) })
}

// loop repeats once with backoff. The backoff restarts only after a session
// that was up for StableAfter: a broker that accepts the connection and drops it
// again keeps being retried slowly.
func (c *Connector) loop(ctx context.Context, once func(up func()) error) error {
	backoff := c.Backoff
	if backoff == nil {
		backoff = ExponentialBackoff
	}
	log := c.logger()
	attempt := 0
	for ctx.Err() == nil {
		var upAt time.Time
		err := once(func() { upAt = c.clock() })
		if ctx.Err() != nil {
			break
		}
		if !upAt.IsZero() && c.clock().Sub(upAt) >= c.stableAfter() {
			attempt = 0
		}
		wait := backoff(attempt)
		attempt++
		log.Warn("rabbitmq connection lost", "connection", c.Name, "error", err.Error(), "retry_in", wait)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return ctx.Err()
}

func (c *Connector) once(ctx context.Context, session Session, up func()) (err error) {
	conn, err := c.dial()(ctx, c.URL.Reveal(), c.Name)
	if err != nil {
		return fmt.Errorf("connect: %w", redact(err, c.URL.Reveal()))
	}
	defer func() {
		if closeErr := conn.CloseDeadline(time.Now().Add(closeTimeout)); closeErr != nil && !errors.Is(closeErr, amqp.ErrClosed) {
			err = errors.Join(err, fmt.Errorf("close: %w", closeErr))
		}
	}()
	closed := conn.NotifyClose(make(chan *amqp.Error, 1))
	sessionCtx, cancel := context.WithCancel(ctx)
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		select {
		case <-sessionCtx.Done():
		case <-closed:
			cancel()
		}
	}()
	defer func() {
		cancel()
		<-watching
	}()
	err = session(sessionCtx, conn, up)
	if err == nil && ctx.Err() == nil {
		err = errors.New("connection closed")
	}
	return err
}

// redactedError is an error whose text has the credentials removed. Unwrap
// returns the original, so errors.Is and errors.As still work.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redact hides the broker URL and its password in the text of err: parser and
// dialer errors may quote the URL.
func redact(err error, rawURL string) error {
	if err == nil || rawURL == "" {
		return err
	}
	msg := err.Error()
	clean := strings.ReplaceAll(msg, rawURL, "<amqp-url>")
	if u, perr := url.Parse(rawURL); perr == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			clean = strings.ReplaceAll(clean, pw, "***")
		}
		if name := u.User.Username(); name != "" {
			clean = strings.ReplaceAll(clean, name+":", "<user>:")
		}
	} else if perr != nil {
		// The URL does not parse; hide what looks like user:password@.
		if at := strings.LastIndex(rawURL, "@"); at > 0 {
			if scheme := strings.Index(rawURL, "://"); scheme >= 0 && scheme+3 < at {
				clean = strings.ReplaceAll(clean, rawURL[scheme+3:at], "<credentials>")
			}
		}
	}
	if clean == msg {
		return err
	}
	return &redactedError{msg: clean, err: err}
}
