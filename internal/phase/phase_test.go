package phase

import "testing"

func TestNoDeletedPhase(t *testing.T) {
	// ADR-017: deletion terminality is the storage tombstone, not a phase.
	if Valid(Phase("deleted")) {
		t.Fatal(`"deleted" must not be a phase`)
	}
	// 05 §7's 8 phases plus docs/12's parked tier (parking/parked/starting).
	if len(All) != 11 {
		t.Fatalf("phase set has %d entries, want 11 (05 §7 + docs/12)", len(All))
	}
}

func TestTransitionTable(t *testing.T) {
	allowed := []struct{ from, to Phase }{
		{Provisioning, Ready},
		{Provisioning, Failed},
		{Ready, Allocated},
		{Allocated, Ready},
		{Ready, Draining},
		{Draining, Ready},     // undrain
		{Draining, Deleting},  // drain complete
		{Failed, Deleting},    // cleanup
		{Allocated, Orphaned}, // provider lost a leased VM; leases stay active
		{Orphaned, Ready},     // re-observed
		{Unknown, Ready},
		// docs/12: the parked tier.
		{Ready, Parking},
		{Parking, Parked},
		{Parking, Ready}, // stop failed: machine still running
		{Parked, Starting},
		{Starting, Ready},  // running + probe
		{Starting, Parked}, // start failed: machine still stopped
		{Starting, Failed}, // started but unhealthy (probe exhausted)
		{Parked, Deleting}, // stage-2 / surplus: direct delete, no drain
		{Parked, Orphaned}, // stopped machine deleted in the cloud console
		{Orphaned, Parked}, // re-observed stopped
	}
	for _, tr := range allowed {
		if !CanTransition(tr.from, tr.to) {
			t.Errorf("legal transition %s -> %s rejected", tr.from, tr.to)
		}
	}
	forbidden := []struct{ from, to Phase }{
		{Provisioning, Allocated}, // must pass through ready (reservation tx)
		{Allocated, Deleting},     // leased resources are never deleted (invariant 3)
		{Deleting, Ready},         // deletes don't resurrect
		{Failed, Ready},           // failed resources are replaced, not revived
		{Ready, Provisioning},
		// docs/12 guards.
		{Allocated, Parking}, // leased machines are never parked (invariant 3 ext.)
		{Parked, Ready},      // parked machines return only through starting
		{Parked, Draining},   // nothing to drain: deletes are direct
		{Parking, Starting},  // must land parked (or revert) first
	}
	for _, tr := range forbidden {
		if CanTransition(tr.from, tr.to) {
			t.Errorf("illegal transition %s -> %s allowed", tr.from, tr.to)
		}
	}
}
