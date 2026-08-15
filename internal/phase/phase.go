// Package phase owns the resource orchestration phase vocabulary (05 §7) and
// its legal-transition table. There is deliberately NO "deleted" phase:
// deletion terminality is the storage tombstone (deleted_at set) — ADR-017.
//
// The full pure transition function phase.Next(Input) grows with the kernel
// increments; the enum and table are frozen here (I2 contract freeze) so
// every component compiles against one vocabulary.
package phase

// Phase is the orchestration phase of a resource — Fleetplane's view, not a
// provider-native machine state.
type Phase string

const (
	Unknown      Phase = "unknown"
	Provisioning Phase = "provisioning"
	Ready        Phase = "ready"
	Allocated    Phase = "allocated"
	Draining     Phase = "draining"
	Deleting     Phase = "deleting"
	Failed       Phase = "failed"
	Orphaned     Phase = "orphaned"

	// Parked-machine tier (docs/12): stop/resume as a warm tier.
	Parking  Phase = "parking"  // stop operation in flight
	Parked   Phase = "parked"   // stopped at the provider, restartable
	Starting Phase = "starting" // start operation in flight
)

// All lists every phase, matching the storage CHECK constraint order.
var All = []Phase{Unknown, Provisioning, Ready, Allocated, Draining, Deleting, Failed, Orphaned, Parking, Parked, Starting}

// Valid reports whether p is a known phase.
func Valid(p Phase) bool {
	for _, q := range All {
		if p == q {
			return true
		}
	}
	return false
}

// transitions is the legal-transition table (kernel design §3.1 as amended
// by plan R3/R8/R9). Tombstoning (deleting→gone, orphaned→gone) is not a
// phase transition: it is ResourceStore.MarkDeleted.
var transitions = map[Phase][]Phase{
	Unknown:      {Ready, Orphaned, Deleting},
	Provisioning: {Ready, Failed, Orphaned},
	Ready:        {Allocated, Draining, Orphaned, Deleting, Parking},
	Allocated:    {Ready, Draining, Orphaned},
	Draining:     {Ready /* undrain */, Deleting},
	Deleting:     {Failed},
	Failed:       {Deleting},
	Orphaned:     {Ready /* re-observed running */, Parked /* re-observed stopped (docs/12) */},

	// docs/12: the parked tier. Parking/Starting are shielded by their
	// active operation; Parked machines can be started, deleted directly
	// (no leases to drain), or orphaned when they vanish at the provider.
	Parking:  {Parked /* stop confirmed */, Ready /* stop failed */},
	Parked:   {Starting, Deleting, Orphaned},
	Starting: {Ready /* running + probe */, Parked /* start failed */, Failed /* started but unhealthy: probe exhausted */},
}

// CanTransition reports whether from → to is in the legal table.
func CanTransition(from, to Phase) bool {
	for _, q := range transitions[from] {
		if q == to {
			return true
		}
	}
	return false
}
