package vpn

import (
	"context"
	"io"
	"log/slog"
	"time"
)

// Retry series after a failed subscription download. Without a fresh cache and
// a running tunnel the series is short and ends at 15 minutes. With both, the
// tunnel keeps working on the cache, so the series starts later.
var (
	retryWithoutCache = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute}
	retryWithCache    = []time.Duration{5 * time.Minute, 10 * time.Minute, 15 * time.Minute}
)

const retryJitter = 0.2

// refreshOutcome is the result of one subscription download attempt. Err is
// kept for the caller only: it may carry the subscription URL, so it is never
// logged.
type refreshOutcome struct {
	OK bool
	// CacheServing is true when a fresh last-known-good config exists and the
	// tunnel is running on it.
	CacheServing bool
	Err          error
}

type clock interface {
	NewTimer(d time.Duration) timer
}

type timer interface {
	C() <-chan time.Time
	Stop() bool
}

type realClock struct{}

func (realClock) NewTimer(d time.Duration) timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.Timer.C }

// refreshScheduler decides when the next subscription download happens. It
// only schedules; it never touches routes or the tunnel, so no branch can open
// a direct route.
type refreshScheduler struct {
	refresh time.Duration
	clock   clock
	// jitter returns a value in [0, 1); it maps to a factor in [0.8, 1.2].
	jitter  func() float64
	logger  *slog.Logger
	attempt int
	next    time.Duration
}

// newRefreshScheduler starts a series with the ordinary refresh interval.
func newRefreshScheduler(refresh time.Duration, clk clock, jitter func() float64, logger *slog.Logger) *refreshScheduler {
	return &refreshScheduler{refresh: refresh, clock: clk, jitter: jitter, logger: logger, next: refresh}
}

// Next is the delay before the next attempt.
func (s *refreshScheduler) Next() time.Duration { return s.next }

// Record stores the outcome of an attempt, logs it, and sets the next delay.
// Success resets the series and returns to the ordinary refresh interval.
func (s *refreshScheduler) Record(outcome refreshOutcome) {
	if outcome.OK {
		s.attempt = 0
		s.next = s.refresh
		s.logger.Info("vpn.refresh.ok")
		return
	}
	s.attempt++
	series := retryWithoutCache
	if outcome.CacheServing {
		series = retryWithCache
	}
	base := series[min(s.attempt, len(series))-1]
	// A retry is never slower than the ordinary interval.
	base = min(base, s.refresh)
	factor := 1 - retryJitter + 2*retryJitter*s.jitter()
	s.next = time.Duration(float64(base) * factor)
	s.logger.Warn("vpn.refresh.failed",
		slog.Int("attempt", s.attempt),
		slog.String("next_in", s.next.Round(time.Second).String()),
		slog.Bool("cache_serving", outcome.CacheServing),
	)
}

// Run waits for each scheduled time and calls attempt. It returns once ctx is
// cancelled and does not start another attempt after that.
func (s *refreshScheduler) Run(ctx context.Context, attempt func(context.Context) refreshOutcome) {
	for {
		t := s.clock.NewTimer(s.next)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
		}
		if ctx.Err() != nil {
			return
		}
		outcome := attempt(ctx)
		if ctx.Err() != nil {
			return
		}
		s.Record(outcome)
	}
}

// newVPNLogger writes JSON log lines, matching the server process.
func newVPNLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, nil))
}
