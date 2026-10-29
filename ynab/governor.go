package ynab

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// YNAB allows 200 requests per rolling hour per access token and exposes no
// usage header (spike F8), so the client counts its own requests.
const (
	DefaultHourlyLimit  = 200
	DefaultWriteReserve = 20
)

// ErrBudgetExhausted is returned, without sending anything, when a request
// would exceed the local rate budget. The governor fails fast rather than
// blocking: a sync tick that can't afford its reads should end and let the
// next tick try, not park for minutes holding a context.
var ErrBudgetExhausted = errors.New("ynab: local request budget exhausted")

// Governor is a token bucket sized to the hourly limit and refilled
// continuously. The last WriteReserve tokens are kept for writes: reads are
// refused once the bucket is down to the reserve. Every HTTP attempt takes a
// token, retries included. It is safe for concurrent use.
type Governor struct {
	mu       sync.Mutex
	now      func() time.Time
	capacity float64
	reserve  float64
	perSec   float64
	tokens   float64
	last     time.Time
}

// NewGovernor returns a full bucket of hourlyLimit tokens, reserve of which
// only writes may use. now is the clock; nil means time.Now.
func NewGovernor(hourlyLimit, reserve int, now func() time.Time) *Governor {
	if now == nil {
		now = time.Now
	}
	hourlyLimit = max(hourlyLimit, 1)
	if reserve < 0 {
		reserve = 0
	}
	if reserve > hourlyLimit {
		reserve = hourlyLimit
	}
	return &Governor{
		now:      now,
		capacity: float64(hourlyLimit),
		reserve:  float64(reserve),
		perSec:   float64(hourlyLimit) / time.Hour.Seconds(),
		tokens:   float64(hourlyLimit),
		last:     now(),
	}
}

// refill tops up tokens for the time since the last call. Callers hold mu.
func (g *Governor) refill() {
	t := g.now()
	if el := t.Sub(g.last).Seconds(); el > 0 {
		g.tokens = min(g.capacity, g.tokens+el*g.perSec)
	}
	g.last = t
}

// floor is the level a request of this kind may not take the bucket below.
func (g *Governor) floor(write bool) float64 {
	if write {
		return 0
	}
	return g.reserve
}

// Take spends one token for a read (write=false) or a write, or returns an
// error wrapping ErrBudgetExhausted that says how long until one is free.
func (g *Governor) Take(write bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refill()
	need := g.floor(write) + 1
	if g.tokens < need {
		// Round up: "in 0s" while still short of a token would be wrong.
		wait := time.Duration(math.Ceil((need-g.tokens)/g.perSec)) * time.Second
		return fmt.Errorf("%w: next %s in %s", ErrBudgetExhausted, kind(write), wait)
	}
	g.tokens--
	return nil
}

// Allow reports, without spending, whether Take(write) would succeed now.
func (g *Governor) Allow(write bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refill()
	if g.tokens < g.floor(write)+1 {
		return fmt.Errorf("%w: no %s budget", ErrBudgetExhausted, kind(write))
	}
	return nil
}

// Remaining returns the whole tokens currently available to reads and to
// writes.
func (g *Governor) Remaining() (reads, writes int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refill()
	return max(0, int(g.tokens-g.reserve)), int(g.tokens)
}

func kind(write bool) string {
	if write {
		return "write"
	}
	return "read"
}
