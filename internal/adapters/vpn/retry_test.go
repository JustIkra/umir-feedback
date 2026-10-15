package vpn

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu      sync.Mutex
	timers  []*fakeTimer
	created chan time.Duration
	// prefired makes every new timer fire at once.
	prefired bool
}

type fakeTimer struct {
	ch chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{created: make(chan time.Duration, 64)} }

func (c *fakeClock) NewTimer(d time.Duration) timer {
	t := &fakeTimer{ch: make(chan time.Time, 1)}
	c.mu.Lock()
	c.timers = append(c.timers, t)
	c.mu.Unlock()
	c.created <- d
	if c.prefired {
		t.ch <- time.Time{}
	}
	return t
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }
func (t *fakeTimer) Stop() bool          { return true }

func (c *fakeClock) fire(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timers[len(c.timers)-1].ch <- time.Time{}
}

func (c *fakeClock) wait(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-c.created:
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not arm a timer")
		return 0
	}
}

func fixedJitter(v float64) func() float64 { return func() float64 { return v } }

func testScheduler(refresh time.Duration, jitter func() float64, out *bytes.Buffer) *refreshScheduler {
	return newRefreshScheduler(refresh, newFakeClock(), jitter, newVPNLogger(out))
}

func TestSchedulerBackoffWithoutFreshCacheCapsAtFifteenMinutes(t *testing.T) {
	s := testScheduler(24*time.Hour, fixedJitter(0.5), &bytes.Buffer{})
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, delay := range want {
		s.Record(refreshOutcome{})
		if s.next != delay {
			t.Fatalf("attempt %d: next = %v, want %v", i+1, s.next, delay)
		}
	}
}

func TestSchedulerRetriesWithFreshCacheAndRunningTunnel(t *testing.T) {
	s := testScheduler(24*time.Hour, fixedJitter(0.5), &bytes.Buffer{})
	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, delay := range want {
		s.Record(refreshOutcome{CacheServing: true})
		if s.next != delay {
			t.Fatalf("attempt %d: next = %v, want %v", i+1, s.next, delay)
		}
	}
}

func TestSchedulerJitterStaysWithinTwentyPercent(t *testing.T) {
	for _, tc := range []struct {
		jitter float64
		want   time.Duration
	}{{0, 24 * time.Second}, {0.5, 30 * time.Second}, {0.999999, 36 * time.Second}} {
		s := testScheduler(24*time.Hour, fixedJitter(tc.jitter), &bytes.Buffer{})
		s.Record(refreshOutcome{})
		if diff := s.next - tc.want; diff < -time.Millisecond || diff > time.Millisecond {
			t.Fatalf("jitter %v: next = %v, want about %v", tc.jitter, s.next, tc.want)
		}
	}
}

func TestSchedulerSuccessResetsSeriesAndReturnsToRefreshInterval(t *testing.T) {
	s := testScheduler(24*time.Hour, fixedJitter(0.5), &bytes.Buffer{})
	s.Record(refreshOutcome{})
	s.Record(refreshOutcome{})
	s.Record(refreshOutcome{OK: true})
	if s.next != 24*time.Hour {
		t.Fatalf("next after success = %v, want 24h", s.next)
	}
	s.Record(refreshOutcome{})
	if s.next != 30*time.Second {
		t.Fatalf("first failure after success = %v, want 30s", s.next)
	}
}

func TestSchedulerFollowsLadderEvenWhenRefreshIntervalIsShort(t *testing.T) {
	s := testScheduler(time.Minute, fixedJitter(0.5), &bytes.Buffer{})
	for range 4 {
		s.Record(refreshOutcome{})
	}
	if s.next != 4*time.Minute {
		t.Fatalf("next = %v, want 4m from the fixed ladder", s.next)
	}
	s.Record(refreshOutcome{OK: true})
	if s.next != time.Minute {
		t.Fatalf("next after success = %v, want the 1m refresh interval", s.next)
	}
}

func TestSchedulerRunWaitsOnClockAndStopsAfterCancel(t *testing.T) {
	clk := newFakeClock()
	s := newRefreshScheduler(24*time.Hour, clk, fixedJitter(0.5), newVPNLogger(&bytes.Buffer{}))
	s.Record(refreshOutcome{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := make(chan struct{}, 8)
	results := []refreshOutcome{{}, {OK: true}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx, func(context.Context) refreshOutcome {
			attempts <- struct{}{}
			out := results[0]
			results = results[1:]
			return out
		})
	}()
	if d := clk.wait(t); d != 30*time.Second {
		t.Fatalf("first wait = %v, want 30s", d)
	}
	clk.fire(t)
	<-attempts
	if d := clk.wait(t); d != time.Minute {
		t.Fatalf("second wait = %v, want 1m", d)
	}
	clk.fire(t)
	<-attempts
	if d := clk.wait(t); d != 24*time.Hour {
		t.Fatalf("wait after success = %v, want 24h", d)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	select {
	case <-attempts:
		t.Fatal("attempt ran after cancel")
	default:
	}
}

func TestSchedulerRunDoesNotAttemptWhenCancelledWithTimerFired(t *testing.T) {
	// The timer is already fired and the context already cancelled; select
	// picks either case at random, so repeat to cover the ctx.Err check after
	// the timer branch.
	for range 200 {
		clk := newFakeClock()
		clk.prefired = true
		s := newRefreshScheduler(time.Hour, clk, fixedJitter(0.5), newVPNLogger(&bytes.Buffer{}))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s.Run(ctx, func(context.Context) refreshOutcome {
			t.Fatal("attempt ran with a cancelled context")
			return refreshOutcome{}
		})
	}
}

func TestSchedulerRunDropsOutcomeOfAttemptCancelledMidway(t *testing.T) {
	clk := newFakeClock()
	clk.prefired = true
	var out bytes.Buffer
	s := newRefreshScheduler(time.Hour, clk, fixedJitter(0.5), newVPNLogger(&out))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Run(ctx, func(context.Context) refreshOutcome {
		cancel()
		return refreshOutcome{}
	})
	if out.Len() != 0 || s.attempt != 0 {
		t.Fatalf("cancelled attempt was recorded: attempt %d, log %q", s.attempt, out.String())
	}
}

func TestSchedulerLogsAttemptAndDelayAsJSONWithoutSubscriptionURL(t *testing.T) {
	const secretURL = "https://subscription.invalid/sub/SECRET-TOKEN-123"
	var out bytes.Buffer
	s := testScheduler(24*time.Hour, fixedJitter(0.5), &out)
	s.Record(refreshOutcome{})
	s.Record(refreshOutcome{CacheServing: true})
	s.Record(refreshOutcome{OK: true})
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("log lines = %d, want 3: %q", len(lines), out.String())
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("log is not JSON: %v", err)
	}
	if first["msg"] != "vpn.refresh.failed" || first["attempt"] != float64(1) || first["next_in"] != "30s" {
		t.Fatalf("failed log = %v", first)
	}
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &last); err != nil {
		t.Fatal(err)
	}
	if last["msg"] != "vpn.refresh.ok" {
		t.Fatalf("ok log = %v", last)
	}
	for _, forbidden := range []string{secretURL, "SECRET-TOKEN", "subscription.invalid", "secret-body"} {
		if strings.Contains(out.String(), forbidden) {
			t.Fatalf("log contains %q: %s", forbidden, out.String())
		}
	}
}
