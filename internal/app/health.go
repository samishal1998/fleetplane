package app

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/samimishal/fleetplane/internal/storage"
	"github.com/samimishal/fleetplane/pkg/sdk"
)

// ProviderHealth is one instance's health surface (07 §8: degraded
// providers are reported here and via metrics — NEVER via readiness).
type ProviderHealth struct {
	Instance            string    `json:"instance"`
	Driver              string    `json:"driver"`
	State               string    `json:"state"` // unknown|healthy|degraded|unavailable
	Since               time.Time `json:"since"`
	LastCheck           time.Time `json:"lastCheck,omitzero"`
	LastError           string    `json:"lastError,omitempty"`
	ConsecutiveFailures int       `json:"consecutiveFailures"`
}

type HealthConfig struct {
	Interval         time.Duration
	DegradedAfter    int // consecutive failures
	UnavailableAfter int
}

func (c *HealthConfig) defaults() {
	if c.Interval <= 0 {
		c.Interval = 30 * time.Second
	}
	if c.DegradedAfter <= 0 {
		c.DegradedAfter = 3
	}
	if c.UnavailableAfter <= 0 {
		c.UnavailableAfter = 10
	}
}

// HealthTracker polls Provider.Health per instance and maintains the
// unknown → healthy → degraded → unavailable state machine.
type HealthTracker struct {
	providers *Providers
	clock     sdk.Clock
	cfg       HealthConfig

	mu     sync.RWMutex
	states map[string]*ProviderHealth
}

func NewHealthTracker(p *Providers, clock sdk.Clock, cfg HealthConfig) *HealthTracker {
	cfg.defaults()
	t := &HealthTracker{providers: p, clock: clock, cfg: cfg, states: map[string]*ProviderHealth{}}
	for _, name := range p.Names() {
		inst, _ := p.Instance(storage.ProviderInstance(name))
		t.states[name] = &ProviderHealth{
			Instance: name, Driver: inst.Descriptor().Driver,
			State: "unknown", Since: clock.Now(),
		}
	}
	return t
}

// CheckAll polls every instance once.
func (t *HealthTracker) CheckAll(ctx context.Context) {
	for _, name := range t.providers.Names() {
		inst, ok := t.providers.Instance(storage.ProviderInstance(name))
		if !ok {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := inst.Health(cctx)
		cancel()
		t.record(name, err)
	}
}

func (t *HealthTracker) record(name string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.states[name]
	if !ok {
		return
	}
	now := t.clock.Now()
	st.LastCheck = now
	if err == nil {
		if st.State != "healthy" {
			st.State, st.Since = "healthy", now
		}
		st.ConsecutiveFailures, st.LastError = 0, ""
		return
	}
	st.ConsecutiveFailures++
	st.LastError = err.Error()
	next := st.State
	switch {
	case st.ConsecutiveFailures >= t.cfg.UnavailableAfter:
		next = "unavailable"
	case st.ConsecutiveFailures >= t.cfg.DegradedAfter:
		next = "degraded"
	}
	if next != st.State {
		st.State, st.Since = next, now
	}
}

// Snapshot returns the current health of every instance, sorted.
func (t *HealthTracker) Snapshot() []ProviderHealth {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]ProviderHealth, 0, len(t.states))
	for _, st := range t.states {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instance < out[j].Instance })
	return out
}

// Run polls until ctx is done.
func (t *HealthTracker) Run(ctx context.Context) {
	t.CheckAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.clock.After(t.cfg.Interval):
			t.CheckAll(ctx)
		}
	}
}
