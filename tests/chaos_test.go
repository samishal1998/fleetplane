package tests

// Nightly chaos run (doc 09 Phase 7): a seeded random schedule of
// acquisitions, releases, pool changes, fault injections, crashes and
// clock jumps — after which the system MUST converge with every invariant
// intact. Gated behind FLEETPLANE_CHAOS=1 (nightly CI); print the seed so
// any failure replays exactly.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/samishal1998/fleetplane/internal/app"
	"github.com/samishal1998/fleetplane/internal/capacity"
	"github.com/samishal1998/fleetplane/internal/reconcile"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
	"github.com/samishal1998/fleetplane/providers/fake"
)

func TestChaos_ConvergesWithInvariantsIntact(t *testing.T) {
	if os.Getenv("FLEETPLANE_CHAOS") != "1" {
		t.Skip("chaos gated: set FLEETPLANE_CHAOS=1 (nightly CI)")
	}
	seed := time.Now().UnixNano()
	if s := os.Getenv("CHAOS_SEED"); s != "" {
		seed, _ = strconv.ParseInt(s, 10, 64)
	}
	t.Logf("chaos seed: %d (replay with CHAOS_SEED=%d)", seed, seed)
	rng := rand.New(rand.NewPCG(uint64(seed), 42))

	h := newHarness(t, fake.Options{CreateSteps: 1, ListLagSteps: 1})
	enableDiscovery(h, reconcile.DiscoveryConfig{OrphanGrace: 10 * time.Second}, nil)
	pool := h.CreatePool("chaos", reconcile.PoolSpec{Class: "ci", Replicas: 1, MaxResources: 8})

	var acqs []storage.AcquisitionID
	steps := 120
	for i := 0; i < steps; i++ {
		switch rng.IntN(8) {
		case 0, 1: // acquire
			id := h.Acquire(app.AcquireCmd{Class: "ci", Constraints: json.RawMessage(cpu1)})
			acqs = append(acqs, id)
		case 2: // release something
			if len(acqs) > 0 {
				idx := rng.IntN(len(acqs))
				_ = h.Svc.Release(context.Background(), string(acqs[idx]), "chaos")
				acqs = append(acqs[:idx], acqs[idx+1:]...)
			}
		case 3: // resize the pool
			h.UpdatePool(pool, "chaos", reconcile.PoolSpec{
				Class: "ci", Replicas: rng.IntN(4), MaxResources: 8,
			})
		case 4: // inject faults
			h.Fake.FailNextApply(provider.ErrRetryable,
				[]provider.SideEffect{provider.EffectNone, provider.EffectMaybe}[rng.IntN(2)],
				1+rng.IntN(2))
		case 5: // rate-limit burst
			h.Fake.RateLimitNext(1+rng.IntN(3), time.Duration(1+rng.IntN(5))*time.Second)
		case 6: // crash and restart
			h.Crash()
			h.Restart()
		case 7: // time passes; sweeps run
			h.Clock.Advance(time.Duration(1+rng.IntN(120)) * time.Second)
			_, _ = h.Leases.SweepExpired(context.Background())
			_ = h.Sched.Resume(context.Background(), (15 * time.Minute).Milliseconds())
			_ = h.Rec.SweepProvider(context.Background(), "fake-local")
		}
		// A few engine/reconciler beats between actions.
		for j := 0; j < 3; j++ {
			_, _ = h.Engine.Step(context.Background())
		}
		_, _ = h.Rec.RunOnce(context.Background(), pool)
		h.Clock.Advance(3 * time.Second)
	}

	// Quiesce: release everything, settle the pool, drain all operations.
	for _, id := range acqs {
		_ = h.Svc.Release(context.Background(), string(id), "chaos")
	}
	h.UpdatePool(pool, "chaos", reconcile.PoolSpec{Class: "ci", Replicas: 2, MaxResources: 8})
	deadline := 400
	for i := 0; ; i++ {
		if i > deadline {
			t.Fatal("chaos never converged")
		}
		_, _ = h.Engine.Step(context.Background())
		d, err := h.Rec.RunOnce(context.Background(), pool)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = h.Rec.ReclaimPoolless(context.Background())
		_ = h.Sched.Resume(context.Background(), (15 * time.Minute).Milliseconds())
		open, _ := h.St.Operations().NonTerminal(context.Background())
		phases := h.PoolPhases(pool)
		if len(open) == 0 && d.Created+d.Deleted+d.Drained+d.Undrained == 0 && phases["ready"] == 2 {
			break
		}
		h.Clock.Advance(9 * time.Second)
	}

	// --- invariant audit ---
	resources, err := h.St.Resources().List(context.Background(), storage.ResourceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	// Invariant 1: no resource is over-allocated.
	for _, r := range resources {
		sum, err := h.St.Leases().SumActive(context.Background(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		total, _ := capacity.Parse(r.Capacity)
		for dim, used := range sum {
			if used > total[dim] {
				t.Fatalf("over-allocated %s on %s: %d > %d (seed %d)", dim, r.ID, used, total[dim], seed)
			}
		}
	}
	// Invariant 7 shadow: every live record with an external id has exactly
	// one cloud object, and no unexplained cloud objects exist.
	cloud := map[string]bool{}
	for _, o := range h.Fake.Objects() {
		cloud[o.Ref.ID] = true
	}
	liveWithExt := 0
	for _, r := range resources {
		if r.ExternalID != nil {
			liveWithExt++
			if !cloud[*r.ExternalID] {
				t.Fatalf("record %s references vanished cloud object %s (seed %d)", r.ID, *r.ExternalID, seed)
			}
		}
	}
	if len(cloud) != liveWithExt {
		t.Fatalf("cloud drift: %d objects vs %d live records (seed %d)", len(cloud), liveWithExt, seed)
	}
	fmt.Printf("chaos: %d steps converged; %d live resources, %d cloud objects (seed %d)\n",
		steps, liveWithExt, len(cloud), seed)
}
