package protocol

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"sync"
	"time"
)

var instantPattern = regexp.MustCompile(`^20[0-9]{2}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var generationPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
var projectPattern = regexp.MustCompile(`^umir-backend-[0-9a-f]{24}$`)

type ClockState struct {
	Owner             string   `json:"owner"`
	InstanceID        string   `json:"instanceID"`
	Source            string   `json:"source"`
	FixtureID         string   `json:"fixtureID"`
	Generation        string   `json:"generation"`
	Instant           string   `json:"instant"`
	InjectedConsumers []string `json:"injectedConsumers"`
	Healthy           bool     `json:"healthy"`
}
type Clock struct {
	mu                sync.Mutex
	instance, fixture string
	instant           time.Time
	generation        uint64
}

func parseInstant(s string) (time.Time, error) {
	at, e := time.Parse(time.RFC3339Nano, s)
	if e != nil || !instantPattern.MatchString(s) || at.Year() < 2000 || at.Year() > 2099 {
		return time.Time{}, errors.New("invalid owner instant")
	}
	return at, nil
}
func NewClock(fixture, instance, instant, generation string) (*Clock, error) {
	at, e := parseInstant(instant)
	n, ne := strconv.ParseUint(generation, 10, 64)
	if e != nil || ne != nil || !generationPattern.MatchString(generation) || !uuidPattern.MatchString(fixture) || !uuidPattern.MatchString(instance) || n > math.MaxInt64 {
		return nil, errors.New("invalid owner clock")
	}
	return &Clock{fixture: fixture, instance: instance, instant: at, generation: n}, nil
}
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.instant.In(time.FixedZone("MSK", 10800))
}
func (c *Clock) Read() ClockState { c.mu.Lock(); defer c.mu.Unlock(); return c.read() }
func (c *Clock) read() ClockState {
	return ClockState{"feedback", c.instance, "owner-injected", c.fixture, strconv.FormatUint(c.generation, 10), c.instant.Format(time.RFC3339Nano), []string{"journal.recovery", "intake.receipt", "jwt.verifier", "worker.claim-finish-retry"}, true}
}
func (c *Clock) Set(instance, generation, instant string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if instance != c.instance || generation != strconv.FormatUint(c.generation, 10) || c.generation >= math.MaxInt64 {
		return "ClockConflict"
	}
	at, e := parseInstant(instant)
	if e != nil {
		return "InvalidRequest"
	}
	if at.Before(c.instant) {
		return "ClockBackward"
	}
	c.instant = at
	c.generation++
	return ""
}
func (c *Clock) ReadExpected(instance, generation string) (ClockState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.read(), c.instance == instance && strconv.FormatUint(c.generation, 10) == generation
}

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
