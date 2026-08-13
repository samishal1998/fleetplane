package hetzner

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"

	"github.com/samimishal/fleetplane/pkg/sdk/provider"
)

// pacer implements the plan-R18 rate policy. Hetzner's RateLimit-Reset
// header marks FULL budget recovery (limit 3600/h refilling 1/s), so
// parking until Reset would freeze operations for up to an hour. Instead:
//
//	Remaining == 0            → park until min(Reset, now+60s)
//	Remaining < lowWater (20) → throttle to the refill rate (1 req/s)
//	Remaining ≥ highWater(100)→ restore the base rate
//	HTTP 429                  → park min(lowWater−Remaining, 60)s, floor 1s
type pacer struct {
	mu      sync.Mutex
	limiter *rate.Limiter
	sem     *semaphore.Weighted
	base    rate.Limit
	parked  time.Time
	now     func() time.Time
}

const (
	lowWater  = 20
	highWater = 100
	parkCap   = 60 * time.Second
)

func newPacer(rps float64, burst, maxConcurrent int, now func() time.Time) *pacer {
	if rps <= 0 {
		rps = 5
	}
	if burst <= 0 {
		burst = 10
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 5
	}
	if now == nil {
		now = time.Now
	}
	return &pacer{
		limiter: rate.NewLimiter(rate.Limit(rps), burst),
		sem:     semaphore.NewWeighted(int64(maxConcurrent)),
		base:    rate.Limit(rps),
		now:     now,
	}
}

// acquire gates one API call; the returned release must be called after.
func (p *pacer) acquire(ctx context.Context) (release func(), err error) {
	p.mu.Lock()
	parked := p.parked
	p.mu.Unlock()
	if until := time.Until(parked); until > 0 {
		return nil, &provider.Error{
			Class: provider.ErrRateLimited, SideEffect: provider.EffectNone,
			Message: "rate limiter parked", RetryAfter: until,
		}
	}
	if err := p.sem.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	if err := p.limiter.Wait(ctx); err != nil {
		p.sem.Release(1)
		return nil, err
	}
	return func() { p.sem.Release(1) }, nil
}

// observe applies the header policy after a successful response.
func (p *pacer) observe(remaining int, reset time.Time) {
	park, limit := decidePace(p.base, remaining, reset, p.now())
	p.mu.Lock()
	defer p.mu.Unlock()
	if !park.IsZero() {
		p.parked = park
	}
	p.limiter.SetLimit(limit)
}

// on429 applies the 429 policy.
func (p *pacer) on429(remaining int) time.Duration {
	d := time.Duration(lowWater-remaining) * time.Second
	if d < time.Second {
		d = time.Second
	}
	if d > parkCap {
		d = parkCap
	}
	p.mu.Lock()
	p.parked = p.now().Add(d)
	p.mu.Unlock()
	return d
}

// decidePace is the pure policy (unit-tested directly).
func decidePace(base rate.Limit, remaining int, reset, now time.Time) (park time.Time, limit rate.Limit) {
	switch {
	case remaining == 0:
		until := reset
		if cap := now.Add(parkCap); until.After(cap) || until.IsZero() {
			until = cap
		}
		return until, 1
	case remaining < lowWater:
		return time.Time{}, 1 // the documented refill rate
	case remaining >= highWater:
		return time.Time{}, base
	default:
		return time.Time{}, 1 // recovering: stay at refill rate until comfortable
	}
}
