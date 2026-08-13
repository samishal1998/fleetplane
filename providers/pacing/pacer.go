package pacing

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
//	Remaining < LowWater (20) → throttle to the refill rate (1 req/s)
//	Remaining ≥ HighWater(100)→ restore the base rate
//	HTTP 429                  → park min(LowWater−Remaining, 60)s, floor 1s
type Pacer struct {
	mu      sync.Mutex
	limiter *rate.Limiter
	sem     *semaphore.Weighted
	base    rate.Limit
	parked  time.Time
	now     func() time.Time
}

const (
	LowWater  = 20
	HighWater = 100
	ParkCap   = 60 * time.Second
)

func New(rps float64, burst, maxConcurrent int, now func() time.Time) *Pacer {
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
	return &Pacer{
		limiter: rate.NewLimiter(rate.Limit(rps), burst),
		sem:     semaphore.NewWeighted(int64(maxConcurrent)),
		base:    rate.Limit(rps),
		now:     now,
	}
}

// acquire gates one API call; the returned release must be called after.
func (p *Pacer) Acquire(ctx context.Context) (release func(), err error) {
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
func (p *Pacer) Observe(remaining int, reset time.Time) {
	park, limit := DecidePace(p.base, remaining, reset, p.now())
	p.mu.Lock()
	defer p.mu.Unlock()
	if !park.IsZero() {
		p.parked = park
	}
	p.limiter.SetLimit(limit)
}

// on429 applies the 429 policy.
func (p *Pacer) On429(remaining int) time.Duration {
	d := time.Duration(LowWater-remaining) * time.Second
	if d < time.Second {
		d = time.Second
	}
	if d > ParkCap {
		d = ParkCap
	}
	p.mu.Lock()
	p.parked = p.now().Add(d)
	p.mu.Unlock()
	return d
}

// decidePace is the pure policy (unit-tested directly).
func DecidePace(base rate.Limit, remaining int, reset, now time.Time) (park time.Time, limit rate.Limit) {
	switch {
	case remaining == 0:
		until := reset
		if cap := now.Add(ParkCap); until.After(cap) || until.IsZero() {
			until = cap
		}
		return until, 1
	case remaining < LowWater:
		return time.Time{}, 1 // the documented refill rate
	case remaining >= HighWater:
		return time.Time{}, base
	default:
		return time.Time{}, 1 // recovering: stay at refill rate until comfortable
	}
}
