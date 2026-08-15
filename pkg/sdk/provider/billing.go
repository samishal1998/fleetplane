package provider

import "time"

// BillingPolicy describes how a provider bills a resource kind (docs/11).
// The zero value means fine-grained billing: no minimum, no increment — the
// kernel then applies ordinary lease/idle policies with no billing-window
// awareness. Billing facts flow one way: the provider states them, the
// kernel's lifecycle policy decides what to do with them (docs/11 §20).
type BillingPolicy struct {
	// MinimumDuration is the shortest period ever billed (0 = none).
	MinimumDuration time.Duration
	// BillingIncrement is the granularity charged after the minimum
	// (0 = per-use/fine-grained).
	BillingIncrement time.Duration
	// TerminationBuffer is how long before a billing boundary a delete
	// should be dispatched so provider-side termination completes inside
	// the paid window. The kernel enforces its own floor on top of this.
	TerminationBuffer time.Duration
}

// FineGrained reports whether the policy carries no billing-window
// information at all.
func (p BillingPolicy) FineGrained() bool {
	return p.MinimumDuration <= 0 && p.BillingIncrement <= 0
}

// BillingAware is the optional capability a Provider implements to expose
// billing characteristics (ADR-013-style: discovered by type assertion, no
// breaking interface change). Kinds the driver does not bill-model return
// the zero policy.
type BillingAware interface {
	Billing(kind ResourceKind) BillingPolicy
}
