package bootstrap

import (
	"context"
	"testing"
	"time"
)

type retentionClock struct{ now time.Time }

func (c *retentionClock) Now() time.Time { return c.now }

type retentionProbe struct{ times []time.Time }

func (p *retentionProbe) PruneChallenges(_ context.Context, now time.Time) error {
	p.times = append(p.times, now)
	return nil
}

type retentionWaiter struct {
	clock  *retentionClock
	cancel context.CancelFunc
	waits  int
}

func (w *retentionWaiter) Wait(ctx context.Context, _ time.Duration) error {
	w.waits++
	if w.waits == 2 {
		w.cancel()
		return ctx.Err()
	}
	w.clock.now = w.clock.now.Add(10 * time.Minute)
	return nil
}

func TestChallengeRetentionNeedsNoOTPKeysOrDeliveryWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &retentionClock{now: time.Date(2027, time.January, 1, 12, 0, 0, 0, time.FixedZone("MSK", 10800))}
	probe := &retentionProbe{}
	waiter := &retentionWaiter{clock: clock, cancel: cancel}
	if err := runChallengeRetention(ctx, probe, clock, waiter, time.Second); err != nil {
		t.Fatal(err)
	}
	if len(probe.times) != 2 || probe.times[1].Sub(probe.times[0]) != 10*time.Minute {
		t.Fatal("challenge cleanup did not follow injected time")
	}
}
