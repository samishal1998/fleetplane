package phase

import "testing"

func TestNoDeletedPhase(t *testing.T) {
	// ADR-017: deletion terminality is the storage tombstone, not a phase.
	if Valid(Phase("deleted")) {
		t.Fatal(`"deleted" must not be a phase`)
	}
	if len(All) != 8 {
		t.Fatalf("phase set has %d entries, want the 8 of 05 §7", len(All))
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
	}
	for _, tr := range forbidden {
		if CanTransition(tr.from, tr.to) {
			t.Errorf("illegal transition %s -> %s allowed", tr.from, tr.to)
		}
	}
}
