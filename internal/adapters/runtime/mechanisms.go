package runtime

import (
	"context"
	"math/rand/v2"
	"time"
	_ "time/tzdata"

	"golang.org/x/text/unicode/norm"
)

type Clock struct{ location *time.Location }

func NewClock() (*Clock, error) {
	location, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		return nil, err
	}
	return &Clock{location: location}, nil
}
func (c *Clock) Now() time.Time { return time.Now().In(c.location) }

type Normalizer struct{}

func (Normalizer) NFC(s string) string { return norm.NFC.String(s) }

type Jitter struct{}

func (Jitter) Delay(base time.Duration) time.Duration {
	return base/2 + time.Duration(rand.Int64N(int64(base/2)+1))
}

type Waiter struct{}

func (Waiter) Wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
