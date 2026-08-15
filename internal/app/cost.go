package app

// CostObserver turns delete-operation terminals into the docs/11 §18 cost
// metrics: termination duration, boundary overruns, and the paid/useful/
// paid-idle accounting (canonical paid(t), merged lease spans). Wired into
// engine.OnTerminal by boot; metrics are best-effort — an error here never
// affects the operation pipeline.

import (
	"context"
	"time"

	"github.com/samishal1998/fleetplane/internal/billing"
	"github.com/samishal1998/fleetplane/internal/metrics"
	"github.com/samishal1998/fleetplane/internal/storage"
)

type CostObserver struct {
	st        storage.Store
	providers *Providers
}

func NewCostObserver(st storage.Store, p *Providers) *CostObserver {
	return &CostObserver{st: st, providers: p}
}

func (c *CostObserver) HandleOpTerminal(op *storage.Operation) {
	if op.Kind != storage.OpKindDelete || op.State != storage.OpSucceeded || op.ResourceID == nil {
		return
	}
	prov := string(op.Provider)
	metrics.TerminationSeconds.WithLabelValues(prov).Observe(float64(op.UpdatedAt-op.CreatedAt) / 1000)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := c.st.Resources().Get(ctx, *op.ResourceID)
	if err != nil {
		return
	}
	pol, _ := c.providers.Billing(op.Provider, res.Kind)
	if pol.FineGrained() {
		return
	}
	// Overrun: the delete was journaled targeting the first boundary after
	// its journal time; confirmation at/after that boundary means another
	// increment was billed for a machine already being destroyed.
	if b, ok := billing.NextBoundary(res.CreatedAt, pol, op.CreatedAt); ok && op.UpdatedAt >= b {
		metrics.BoundaryOverruns.WithLabelValues(prov).Inc()
	}

	lifetime := time.Duration(op.UpdatedAt-res.CreatedAt) * time.Millisecond
	paid := billing.Paid(lifetime, pol)
	var spans []billing.Interval
	if leases, err := c.st.Leases().ByResource(ctx, res.ID); err == nil {
		for _, l := range leases {
			iv := billing.Interval{StartMs: l.CreatedAt}
			if l.EndedAt != nil {
				iv.EndMs = *l.EndedAt
			}
			spans = append(spans, iv)
		}
	}
	useful := time.Duration(billing.UsefulMillis(spans, op.UpdatedAt)) * time.Millisecond
	idle := paid - useful
	if idle < 0 {
		idle = 0
	}
	metrics.PaidSeconds.WithLabelValues(prov, res.Kind).Add(paid.Seconds())
	metrics.UsefulSeconds.WithLabelValues(prov, res.Kind).Add(useful.Seconds())
	metrics.PaidIdleSeconds.WithLabelValues(prov, res.Kind).Add(idle.Seconds())
}
