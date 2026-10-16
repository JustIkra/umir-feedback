package itest

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"os"
	"testing"
)

// repo names this repository in the temporary names of databases, virtual
// hosts and Redis prefixes: t_<repo>_<rand> and t:<repo>:<rand>:.
const repo = "feedback"

// fataler is the part of testing.TB that requireEnv needs; a test double can
// implement it.
type fataler interface {
	Fatalf(format string, args ...any)
	Helper()
}

// requireEnv returns the value of an environment variable of the durable test
// infrastructure. A missing variable fails the test: the integration suite
// never skips. The message names the variable only, never a value.
func requireEnv(tb fataler, name string) string {
	tb.Helper()
	value := os.Getenv(name)
	if value == "" {
		tb.Fatalf("%s is not set: the integration tests need the durable test infrastructure; "+
			"run `bash scripts/durable-test-infra.sh up` once and `eval \"$(bash scripts/durable-test-infra.sh env)\"` "+
			"in the umir-factory checkout (the suite never skips)", name)
	}
	return value
}

// randHex returns 8 random hexadecimal characters.
func randHex(tb fataler) string {
	tb.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		tb.Fatalf("random id: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// ignoreErr documents a deliberately ignored error from a shutdown path, where
// the peer may already have closed the connection.
func ignoreErr(error) {}

// DatabaseURL returns the administrative PostgreSQL URL from
// UMIR_TEST_DATABASE_URL. Tests do not use it for their data: they call
// NewDatabase.
func DatabaseURL(tb testing.TB) string {
	tb.Helper()
	return requireEnv(tb, "UMIR_TEST_DATABASE_URL")
}

// AMQPURL returns the RabbitMQ URL of the default virtual host from
// UMIR_TEST_AMQP_URL. Tests do not use it for their data: they call NewVHost.
func AMQPURL(tb testing.TB) string {
	tb.Helper()
	return requireEnv(tb, "UMIR_TEST_AMQP_URL")
}

// RedisLimiterURL returns the URL of the limiter Redis (AOF, noeviction) from
// UMIR_TEST_REDIS_LIMITER_URL.
func RedisLimiterURL(tb testing.TB) string {
	tb.Helper()
	return requireEnv(tb, "UMIR_TEST_REDIS_LIMITER_URL")
}

// RedisCacheURL returns the URL of the cache Redis (allkeys-lru, no
// persistence) from UMIR_TEST_REDIS_CACHE_URL.
func RedisCacheURL(tb testing.TB) string {
	tb.Helper()
	return requireEnv(tb, "UMIR_TEST_REDIS_CACHE_URL")
}

// deriveManagementURL maps the local test broker AMQP URL to its management
// URL. It refuses anything else, because guessing the management port of a
// foreign broker could touch data that is not ours. Errors never contain the
// URL, which carries credentials.
func deriveManagementURL(amqpURL string) (string, error) {
	u, err := url.Parse(amqpURL)
	if err != nil {
		return "", errors.New("UMIR_TEST_AMQP_URL is not a valid URL")
	}
	if u.Hostname() != "127.0.0.1" || u.Port() != "55672" {
		return "", errors.New("cannot derive the RabbitMQ management URL from UMIR_TEST_AMQP_URL: " +
			"set UMIR_TEST_AMQP_MANAGEMENT_URL (the infra script prints it with `env`)")
	}
	m := url.URL{Scheme: "http", User: u.User, Host: net.JoinHostPort("127.0.0.1", "55673")}
	return m.String(), nil
}
