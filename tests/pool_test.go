package tests

// Pool reconciliation tests (doc 05; Phase-4 exit: replica changes converge
// without duplicate creates). Invariant 4 is pinned in both directions.

import (
	"context"
	"errors"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/samishal1998/fleetplane/internal/app"
	"github.com/samishal1998/fleetplane/internal/phase"
	"github.com/samishal1998/fleetplane/internal/reconcile"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/pkg/kinds/compute"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
	"github.com/samishal1998/fleetplane/providers/fake"
)

func TestPool_ConvergesToReplicas(t *testing.T) {
	h := newHarness(t, fake.Options{})
	pool := h.CreatePool("ci", reconcile.PoolSpec{Class: "ci", Replicas: 3})

	h.Converge(pool)
	if got := h.PoolPhases(pool); got["ready"] != 3 {
		t.Fatalf("pool phases = %v, want 3 ready", got)
	}
	if n := h.CloudObjects(); n != 3 {
		t.Fatalf("cloud has %d objects, want 3", n)
	}

	// Re-running decides nothing (fixed point; no duplicate creates).
	d, err := h.Rec.RunOnce(context.Background(), pool)
	if err != nil || d.Created != 0 || d.Deleted != 0 {
		t.Fatalf("fixed point violated: %+v %v", d, err)
	}
}

// Invariant 4 both directions: provisioning rows count as pending creates;
// failed rows do not.
func TestInv4_PendingCreatesCounted(t *testing.T) {
	h := newHarness(t, fake.Options{})
	pool := h.CreatePool("ci", reconcile.PoolSpec{Class: "ci", Replicas: 4})

	// First cycle journals 4 creates (undriven → provisioning).
	d, err := h.Rec.RunOnce(context.Background(), pool)
	if err != nil || d.Created != 4 {
		t.Fatalf("first cycle created %d, want 4 (%v)", d.Created, err)
	}
	// Second cycle with 4 provisioning rows must create ZERO more.
	d, err = h.Rec.RunOnce(context.Background(), pool)
	if err != nil || d.Created != 0 {
		t.Fatalf("pending creates not counted: created %d more (invariant 4)", d.Created)
	}

	// Near-miss: failed resources are NOT counted — they get replaced.
	h.DriveAll()
	list, _ := h.St.Resources().List(context.Background(), storage.ResourceFilter{PoolID: &pool})
	victim := list[0]
	if err := h.St.Tx(context.Background(), func(tx storage.TxStore) error {
		return tx.Resources().CASPhase(context.Background(), victim.ID, phase.Ready, phase.Orphaned, h.Clock.Now().UnixMilli())
	}); err != nil {
		t.Fatal(err)
	}
	// orphaned is outside {provisioning, ready, allocated} → deficit 1.
	d, err = h.Rec.RunOnce(context.Background(), pool)
	if err != nil || d.Created != 1 {
		t.Fatalf("non-counted phase not replaced: created %d, want 1 (%v)", d.Created, err)
	}
}

func TestPool_ScaleDownDrainsThenDeletes(t *testing.T) {
	h := newHarness(t, fake.Options{})
	pool := h.CreatePool("ci", reconcile.PoolSpec{Class: "ci", Replicas: 3})
	h.Converge(pool)

	h.UpdatePool(pool, "ci", reconcile.PoolSpec{Class: "ci", Replicas: 1})
	h.Converge(pool)

	got := h.PoolPhases(pool)
	if got["ready"] != 1 || got["draining"] != 0 || got["deleting"] != 0 {
		t.Fatalf("after scale-down: %v, want exactly 1 ready", got)
	}
	if n := h.CloudObjects(); n != 1 {
		t.Fatalf("cloud has %d objects, want 1 (scale-down uses drain→delete, 05 §6)", n)
	}
}

func TestPool_UndrainWhenDeficitReturns(t *testing.T) {
	h := newHarness(t, fake.Options{})
	pool := h.CreatePool("ci", reconcile.PoolSpec{Class: "ci", Replicas: 2})
	h.Converge(pool)

	// Shrink to 1: one resource starts draining.
	h.UpdatePool(pool, "ci", reconcile.PoolSpec{Class: "ci", Replicas: 1})
	d, err := h.Rec.RunOnce(context.Background(), pool)
	if err != nil || d.Drained != 1 {
		t.Fatalf("drain cycle: %+v %v", d, err)
	}
	before := h.CloudObjects()

	// Grow back to 2 before the drain pipeline deletes: undrain, no
	// delete op, no new create.
	h.UpdatePool(pool, "ci", reconcile.PoolSpec{Class: "ci", Replicas: 2})
	d, err = h.Rec.RunOnce(context.Background(), pool)
	if err != nil || d.Undrained != 1 || d.Created != 0 || d.Deleted != 0 {
		t.Fatalf("undrain cycle: %+v %v", d, err)
	}
	if h.CloudObjects() != before {
		t.Fatal("undrain touched the cloud")
	}
	if got := h.PoolPhases(pool); got["ready"] != 2 {
		t.Fatalf("after undrain: %v", got)
	}
}

func TestPool_MaxResourcesCapsAndMinReadyHolds(t *testing.T) {
	h := newHarness(t, fake.Options{})
	pool := h.CreatePool("ci", reconcile.PoolSpec{Class: "ci", Replicas: 5, MaxResources: 3})
	h.Converge(pool)
	if got := h.PoolPhases(pool); got["ready"] != 3 {
		t.Fatalf("maxResources not enforced: %v", got)
	}

	// minReady blocks draining below the floor even when surplus exists.
	h.UpdatePool(pool, "ci", reconcile.PoolSpec{Class: "ci", Replicas: 0, MinReady: 2, MaxResources: 3})
	h.Converge(pool)
	if got := h.PoolPhases(pool); got["ready"] < 2 {
		t.Fatalf("minReady floor broken: %v", got)
	}
}

// PRD success metric: reconciliation converges after transient provider
// failures — with persisted failure backoff, no thrash, no duplicates.
func TestPool_ConvergesUnderTransientFailures(t *testing.T) {
	h := newHarness(t, fake.Options{})
	h.Fake.FailNextApply(provider.ErrRetryable, provider.EffectNone, 3)
	pool := h.CreatePool("ci", reconcile.PoolSpec{Class: "ci", Replicas: 2})

	h.Converge(pool)
	if got := h.PoolPhases(pool); got["ready"] != 2 {
		t.Fatalf("did not converge past transient failures: %v", got)
	}
	if n := h.CloudObjects(); n != 2 {
		t.Fatalf("cloud has %d objects, want 2 (no duplicates)", n)
	}
}

func TestPool_IdleReclaimRespectsIdleAfter(t *testing.T) {
	h := newHarness(t, fake.Options{})
	pool := h.CreatePool("ci", reconcile.PoolSpec{
		Class: "ci", Replicas: 2,
		Reclaim: &reconcile.ReclaimPolicy{IdleAfter: compute.Duration(10 * time.Minute)},
	})
	h.Converge(pool)

	// Shrink: resources are NOT idle long enough — nothing drains yet.
	h.UpdatePool(pool, "ci", reconcile.PoolSpec{
		Class: "ci", Replicas: 1,
		Reclaim: &reconcile.ReclaimPolicy{IdleAfter: compute.Duration(10 * time.Minute)},
	})
	d, err := h.Rec.RunOnce(context.Background(), pool)
	if err != nil || d.Drained != 0 {
		t.Fatalf("drained before idleAfter elapsed: %+v %v", d, err)
	}
	// After idleAfter passes, the surplus drains.
	h.Clock.Advance(11 * time.Minute)
	h.Converge(pool)
	if got := h.PoolPhases(pool); got["ready"] != 1 {
		t.Fatalf("idle surplus not reclaimed: %v", got)
	}
}

// Property: any sequence of replica changes with random transient faults
// converges to exactly `replicas` ready resources and an identical cloud
// count — never exceeding maxResources along the way.
func TestPropPool_Converges(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		h := newHarness(t, fake.Options{})
		const maxRes = 6
		pool := h.CreatePool("ci", reconcile.PoolSpec{Class: "ci", Replicas: 0, MaxResources: maxRes})

		steps := rapid.IntRange(1, 4).Draw(rt, "steps")
		var replicas int
		for i := 0; i < steps; i++ {
			replicas = rapid.IntRange(0, 5).Draw(rt, "replicas")
			if rapid.Bool().Draw(rt, "fault") {
				h.Fake.FailNextApply(provider.ErrRetryable, provider.EffectNone, rapid.IntRange(1, 2).Draw(rt, "failN"))
			}
			h.UpdatePool(pool, "ci", reconcile.PoolSpec{Class: "ci", Replicas: replicas, MaxResources: maxRes})
			h.Converge(pool)

			list, err := h.St.Resources().List(context.Background(), storage.ResourceFilter{PoolID: &pool})
			if err != nil {
				rt.Fatal(err)
			}
			if len(list) > maxRes {
				rt.Fatalf("maxResources exceeded: %d live rows", len(list))
			}
		}
		got := h.PoolPhases(pool)
		if got["ready"] != replicas {
			rt.Fatalf("converged to %v, want %d ready", got, replicas)
		}
		if n := h.CloudObjects(); n != replicas {
			rt.Fatalf("cloud has %d, want %d", n, replicas)
		}
	})
}

// Pause has to gate convergence itself, not just decorate the pool row: the
// reconciler read Pool.Paused long before anything could set it, so the only
// proof the verb works is a replica deficit that stays unfilled.
func TestPool_PausedReconcilerCreatesNothing(t *testing.T) {
	h := newHarness(t, fake.Options{})
	ctx := context.Background()
	pool := h.CreatePool("ci", reconcile.PoolSpec{Class: "ci", Replicas: 3})

	if err := h.Svc.SetPoolPaused(ctx, string(pool), true, "test"); err != nil {
		t.Fatal(err)
	}
	d, err := h.Rec.RunOnce(ctx, pool)
	if err != nil || d.Created != 0 {
		t.Fatalf("paused cycle decided %+v (%v), want nothing created", d, err)
	}
	h.DriveAll()
	if got := h.PoolPhases(pool); len(got) != 0 || h.CloudObjects() != 0 {
		t.Fatalf("paused pool touched the fleet: %v, %d cloud objects", got, h.CloudObjects())
	}
	// A kick is refused rather than swallowed by the paused cycle.
	if err := h.Svc.ReconcilePool(ctx, string(pool)); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("reconcile while paused: %v, want a conflict", err)
	}
	// Re-pausing is success, not a conflict.
	if err := h.Svc.SetPoolPaused(ctx, string(pool), true, "test"); !errors.Is(err, app.ErrAlreadyThere) {
		t.Fatalf("re-pause: %v, want ErrAlreadyThere", err)
	}

	if err := h.Svc.SetPoolPaused(ctx, string(pool), false, "test"); err != nil {
		t.Fatal(err)
	}
	h.Converge(pool)
	if got := h.PoolPhases(pool); got["ready"] != 3 {
		t.Fatalf("after resume: %v, want 3 ready", got)
	}
}

// Deleting a pool is gated twice, and both gates guard against an orphan: a
// pool with replicas left is one the reconciler is still creating into, and
// live members would lose the row their pool_id points at. The success case
// runs after a scale-down, so the members are tombstones that still carry
// pool_id — the FK case that a naive DELETE cannot do.
func TestPool_DeleteGatesOnReplicasAndMembers(t *testing.T) {
	h := newHarness(t, fake.Options{})
	ctx := context.Background()
	pool := h.CreatePool("ci", reconcile.PoolSpec{Class: "ci", Replicas: 1})

	// replicas > 0, nothing created yet.
	if err := h.Svc.DeletePool(ctx, string(pool), "test"); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("delete with replicas > 0: %v, want a conflict", err)
	}
	h.Converge(pool)

	// Scaled to zero is not enough while the member is still live.
	h.UpdatePool(pool, "ci", reconcile.PoolSpec{Class: "ci", Replicas: 0})
	if err := h.Svc.DeletePool(ctx, string(pool), "test"); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("delete with a live member: %v, want a conflict", err)
	}

	h.Converge(pool)
	if got := h.PoolPhases(pool); len(got) != 0 {
		t.Fatalf("members still live after scale-down: %v", got)
	}
	if err := h.Svc.DeletePool(ctx, string(pool), "test"); err != nil {
		t.Fatalf("delete of an empty scaled-to-zero pool: %v", err)
	}
	if _, err := h.Svc.GetPool(ctx, string(pool)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("pool still readable after delete: %v", err)
	}
}
