package provider

import "time"

// ParkPolicy describes a provider's stop/resume support for a kind
// (docs/12). The zero value (Supported=false) means machines of this kind
// cannot be parked — reclaim falls back to delete/create.
type ParkPolicy struct {
	Supported bool
	// StartEstimate is a hint for the typical stopped->running latency,
	// used for queue-wait estimation until observed data exists.
	StartEstimate time.Duration
}

// ParkAware is the optional capability a Provider implements to expose
// stop/resume (ADR-013 style, discovered by type assertion). Drivers
// implementing it accept the "stop" and "start" Action kinds under a hard
// contract: BOTH ARE IDEMPOTENT — stopping a stopped machine and starting
// a running machine return success — which is what makes crash-duplicated
// dispatch harmless (docs/12 §7). Kinds without support return the zero
// policy.
type ParkAware interface {
	Parking(kind ResourceKind) ParkPolicy
}
