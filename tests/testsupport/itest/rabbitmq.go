package itest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

const vhostPrefix = "t_" + repo + "_"

// management is a minimal client of the RabbitMQ management HTTP API. It only
// ever touches virtual hosts named t_feedback_*.
type management struct {
	base     url.URL // no credentials
	user     string
	password string
	client   *http.Client
}

func newManagement(tb testing.TB) *management {
	tb.Helper()
	raw := os.Getenv("UMIR_TEST_AMQP_MANAGEMENT_URL")
	if raw == "" {
		derived, err := deriveManagementURL(AMQPURL(tb))
		if err != nil {
			tb.Fatalf("%v", err)
		}
		raw = derived
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		tb.Fatalf("UMIR_TEST_AMQP_MANAGEMENT_URL is not a valid URL")
	}
	user, password := "", ""
	if u.User != nil {
		user = u.User.Username()
		password, _ = u.User.Password()
	}
	if user == "" {
		// Fall back to the AMQP credentials: the test broker uses one account.
		if a, perr := url.Parse(AMQPURL(tb)); perr == nil && a.User != nil {
			user = a.User.Username()
			password, _ = a.User.Password()
		}
	}
	u.User = nil
	return &management{base: *u, user: user, password: password, client: &http.Client{Timeout: 15 * time.Second}}
}

// do sends one request and returns the status code. The response body is
// drained and dropped: it is never put into an error.
func (m *management) do(ctx context.Context, method string, segments []string, body any) (int, error) {
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	target := m.base.JoinPath(segments...)
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return 0, fmt.Errorf("build management request: %w", err)
	}
	req.SetBasicAuth(m.user, m.password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("management %s %s: %w", method, strings.Join(segments, "/"), err)
	}
	defer ignoreBody(resp)
	return resp.StatusCode, nil
}

func ignoreBody(resp *http.Response) {
	ignoreErr(resp.Body.Close())
}

func (m *management) createVHost(ctx context.Context, name, user string) error {
	if !strings.HasPrefix(name, vhostPrefix) {
		return fmt.Errorf("refusing to create virtual host %q: not a test name", name)
	}
	status, err := m.do(ctx, http.MethodPut, []string{"api", "vhosts", name}, struct{}{})
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusNoContent {
		return fmt.Errorf("create virtual host %s: management answered %d", name, status)
	}
	perms := map[string]string{"configure": ".*", "write": ".*", "read": ".*"}
	status, err = m.do(ctx, http.MethodPut, []string{"api", "permissions", name, user}, perms)
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusNoContent {
		return fmt.Errorf("grant permissions on %s: management answered %d", name, status)
	}
	return nil
}

func (m *management) deleteVHost(ctx context.Context, name string) error {
	if !strings.HasPrefix(name, vhostPrefix) {
		return fmt.Errorf("refusing to delete virtual host %q: not a test name", name)
	}
	status, err := m.do(ctx, http.MethodDelete, []string{"api", "vhosts", name}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusNotFound {
		return fmt.Errorf("delete virtual host %s: management answered %d", name, status)
	}
	return nil
}

// NewVHost creates a temporary virtual host t_feedback_<rand> through the
// management API, grants the account of UMIR_TEST_AMQP_URL full permissions on
// it and returns the AMQP URL of that virtual host. The virtual host is
// deleted when the test ends. Only virtual hosts created here are ever deleted.
func NewVHost(tb testing.TB) string {
	tb.Helper()
	amqpURL := AMQPURL(tb)
	u, err := url.Parse(amqpURL)
	if err != nil || u.User == nil || u.User.Username() == "" {
		tb.Fatalf("UMIR_TEST_AMQP_URL must be an amqp URL with a user name")
	}
	m := newManagement(tb)
	name := vhostPrefix + randHex(tb)
	ctx, cancel := context.WithTimeout(tb.Context(), cleanupTimeout)
	defer cancel()
	// Cleanup is registered first: a half-created virtual host is removed too.
	tb.Cleanup(func() {
		delCtx, delCancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer delCancel()
		if err := m.deleteVHost(delCtx, name); err != nil {
			tb.Errorf("delete test virtual host %s: %v", name, err)
		}
	})
	if err := m.createVHost(ctx, name, u.User.Username()); err != nil {
		tb.Fatalf("%v", err)
	}
	u.Path = "/" + name
	u.RawPath = ""
	return u.String()
}
