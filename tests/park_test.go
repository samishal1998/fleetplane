package tests

// docs/12 proofs: park at idle, start-on-acquire reuse, stage-2 delete,
// queued-work protection of parked machines, start-failure fall-through,
// pool warm tier, crash-resume of stop/start, and the explicit API verbs.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/samishal1998/fleetplane/internal/app"
	"github.com/samishal1998/fleetplane/internal/capacity"
	"github.com/samishal1998/fleetplane/internal/ids"
	"github.com/samishal1998/fleetplane/internal/phase"
	"github.com/samishal1998/fleetplane/internal/provision"
	"github.com/samishal1998/fleetplane/internal/reconcile"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
	"github.com/samishal1998/fleetplane/providers/fake"
)

func parkFake() fake.Options { return fake.Options{Park: true} }

// parkedMachine acquires+releases a ci-park machine and drives it through
// stage-1 parking; returns the resource.
func parkedMachine(t *testing.T, h *Harness) *storage.Resource {
	t.Helper()
	ctx := context.Background()
	a := h.Acquire(app.AcquireCmd{Class: "ci-park", TTL: time.Minute})
	h.DriveAll()
	if err := h.Svc.Release(ctx, string(a), "test"); err != nil {
		t.Fatal(err)
	}
	h.Clock.Advance(11 * time.Minute)
	if n, _ := h.Rec.ReclaimPoolless(ctx); n != 1 {
		t.Fatal("stage-1 park not journaled")
	}
	h.DriveAll()
	list, _ := h.St.Resources().List(ctx, storage.ResourceFilter{Class: "ci-park"})
	if len(list) != 1 || list[0].Phase != phase.Parked || list[0].ParkedAt == nil {
		t.Fatalf("machine not parked: %+v", list[0])
	}
	if h.CloudObjects() != 1 {
		t.Fatalf("park must STOP, not delete: %d cloud objects", h.CloudObjects())
	}
	return list[0]
}

// Stage 1 parks instead of deleting; the next acquisition starts the parked
// machine instead of creating (docs/12 §5–6).
func TestParkAtIdleAndStartOnAcquire(t *testing.T) {
	h := newHarness(t, parkFake())
	res := parkedMachine(t, h)

	b := h.Acquire(app.AcquireCmd{Class: "ci-park", TTL: time.Minute})
	if got := h.acq(b).State; got != storage.AcqProvisioning {
		t.Fatalf("acquisition should be starting the parked machine, state=%s", got)
	}
	h.DriveAll()
	acq := h.acq(b)
	if acq.State != storage.AcqBound {
		t.Fatalf("state after start = %s", acq.State)
	}
	l, err := h.St.Leases().Get(context.Background(), *acq.LeaseID)
	if err != nil || l.ResourceID != res.ID {
		t.Fatalf("bound to %v, want the previously parked %s", l, res.ID)
	}
	cur, _ := h.St.Resources().Get(context.Background(), res.ID)
	if cur.Phase != phase.Allocated || cur.ParkedAt != nil {
		t.Fatalf("after start+bind: phase=%s parkedAt=%v", cur.Phase, cur.ParkedAt)
	}
	if h.CloudObjects() != 1 {
		t.Fatalf("start-before-create violated: %d machines", h.CloudObjects())
	}
}

// Stage 2: parked past deleteAfter is deleted (docs/12 §5).
func TestParkedStage2DeleteAfter(t *testing.T) {
	h := newHarness(t, parkFake())
	res := parkedMachine(t, h)
	ctx := context.Background()

	h.Clock.Advance(time.Hour) // parked 1h < 2h: kept
	if n, _ := h.Rec.ReclaimPoolless(ctx); n != 0 {
		t.Fatal("stage-2 fired before deleteAfter")
	}
	h.Clock.Advance(90 * time.Minute) // now > 2h parked
	if n, _ := h.Rec.ReclaimPoolless(ctx); n != 1 {
		t.Fatal("stage-2 delete not journaled")
	}
	h.DriveAll()
	if h.CloudObjects() != 0 {
		t.Fatalf("cloud objects = %d after stage-2", h.CloudObjects())
	}
	cur, _ := h.St.Resources().Get(ctx, res.ID)
	if cur.DeletedAt == nil {
		t.Fatal("no tombstone after stage-2 delete")
	}
}

// A queued compatible acquisition protects a parked machine from stage-2 —
// rung 2 can start it in seconds (docs/12 invariant 5).
func TestQueuedWorkProtectsParkedFromStage2(t *testing.T) {
	h := newHarness(t, parkFake())
	_ = parkedMachine(t, h)
	ctx := context.Background()
	h.Clock.Advance(3 * time.Hour)

	nowMs := h.Clock.Now().UnixMilli()
	acqID := storage.AcquisitionID(ids.New(ids.Acquisition))
	req, _ := json.Marshal(capacity.Request{MaxWaitMs: (30 * time.Minute).Milliseconds()})
	if err := h.St.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Acquisitions().Insert(ctx, &storage.Acquisition{
			ID: acqID, Actor: "test", Kind: "compute.machine", Class: "ci-park",
			Constraints: req, Quantity: 1, State: storage.AcqPending,
			CreatedAt: nowMs, UpdatedAt: nowMs,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := h.Rec.ReclaimPoolless(ctx); n != 0 {
		t.Fatal("stage-2 deleted the machine queued work waits for")
	}
	_ = h.St.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Acquisitions().Transition(ctx, acqID, storage.AcqPending, storage.AcqReleased, h.Clock.Now().UnixMilli())
	})
	if n, _ := h.Rec.ReclaimPoolless(ctx); n != 1 {
		t.Fatal("stage-2 blocked after the queued work withdrew")
	}
}

// A failed start reverts the machine to parked, re-pends the acquisition,
// and the backoff makes the ladder fall through to a create (docs/12 §4/§6).
func TestStartFailureFallsThroughToCreate(t *testing.T) {
	h := newHarness(t, parkFake())
	res := parkedMachine(t, h)
	ctx := context.Background()

	h.Fake.FailNextApply(provider.ErrInvalid, provider.EffectNone, 1)
	b := h.Acquire(app.AcquireCmd{Class: "ci-park", TTL: time.Minute})
	h.DriveAll() // start op fails -> machine parked again, acquisition re-pended

	cur, _ := h.St.Resources().Get(ctx, res.ID)
	if cur.Phase != phase.Parked {
		t.Fatalf("failed start must revert to parked, got %s", cur.Phase)
	}
	if got := h.acq(b).State; got != storage.AcqPending {
		t.Fatalf("acquisition after failed start = %s, want pending", got)
	}
	// The sweep retries: backoff skips the broken machine, a create runs.
	if err := h.Sched.Resume(ctx, (15 * time.Minute).Milliseconds()); err != nil {
		t.Fatal(err)
	}
	h.DriveAll()
	if got := h.acq(b).State; got != storage.AcqBound {
		t.Fatalf("acquisition after fall-through = %s", got)
	}
	if h.CloudObjects() != 2 {
		t.Fatalf("expected a second machine after fall-through, got %d", h.CloudObjects())
	}
}

// Pool warm tier (docs/12 §8): idle machines park down to minRunning, a
// replica reduction consumes the parked tier first, and warm-up starts
// parked machines before creating.
func TestPoolWarmTier(t *testing.T) {
	h := newHarness(t, parkFake())
	ctx := context.Background()
	poolID := h.CreatePool("warm", reconcile.PoolSpec{Class: "ci-park", Replicas: 3, MinRunning: 1})
	h.Converge(poolID)
	if got := h.PoolPhases(poolID)["ready"]; got != 3 {
		t.Fatalf("initial fleet: ready=%d want 3 (deficit creates hot)", got)
	}

	// Idle past idleAfter: the park pass takes hot down to minRunning.
	h.Clock.Advance(11 * time.Minute)
	if _, err := h.Rec.RunOnce(ctx, poolID); err != nil {
		t.Fatal(err)
	}
	h.DriveAll()
	ph := h.PoolPhases(poolID)
	if ph["ready"] != 1 || ph["parked"] != 2 {
		t.Fatalf("warm tier: %v, want 1 ready + 2 parked", ph)
	}
	if h.CloudObjects() != 3 {
		t.Fatalf("parking must not delete: %d cloud objects", h.CloudObjects())
	}

	// Replica reduction consumes the PARKED tier, never the hot machine.
	h.UpdatePool(poolID, "warm", reconcile.PoolSpec{Class: "ci-park", Replicas: 2, MinRunning: 1})
	if _, err := h.Rec.RunOnce(ctx, poolID); err != nil {
		t.Fatal(err)
	}
	h.DriveAll()
	ph = h.PoolPhases(poolID)
	if ph["ready"] != 1 || ph["parked"] != 1 {
		t.Fatalf("after replicas 3->2: %v, want 1 ready + 1 parked", ph)
	}

	// Raising minRunning warms up from the parked tier, not via creates.
	h.UpdatePool(poolID, "warm", reconcile.PoolSpec{Class: "ci-park", Replicas: 2, MinRunning: 2})
	if _, err := h.Rec.RunOnce(ctx, poolID); err != nil {
		t.Fatal(err)
	}
	h.DriveAll()
	ph = h.PoolPhases(poolID)
	if ph["ready"] != 2 || ph["parked"] != 0 {
		t.Fatalf("warm-up: %v, want 2 ready", ph)
	}
	if h.CloudObjects() != 2 {
		t.Fatalf("warm-up must start, not create: %d cloud objects", h.CloudObjects())
	}
}

// kill -9 mid-stop and mid-start: the idempotent ops resume and land.
func TestParkCrashResume(t *testing.T) {
	h := newHarness(t, parkFake())
	ctx := context.Background()
	a := h.Acquire(app.AcquireCmd{Class: "ci-park", TTL: time.Minute})
	h.DriveAll()
	_ = h.Svc.Release(ctx, string(a), "test")
	h.Clock.Advance(11 * time.Minute)
	if n, _ := h.Rec.ReclaimPoolless(ctx); n != 1 {
		t.Fatal("park not journaled")
	}

	h.Crash()
	h.Restart()
	h.DriveAll() // the journaled stop resumes and completes
	list, _ := h.St.Resources().List(ctx, storage.ResourceFilter{Class: "ci-park"})
	if len(list) != 1 || list[0].Phase != phase.Parked {
		t.Fatalf("after crash-resume: %+v", list[0])
	}

	// Now crash mid-start.
	b := h.Acquire(app.AcquireCmd{Class: "ci-park", TTL: time.Minute})
	h.Crash()
	h.Restart()
	if err := h.Sched.Resume(ctx, (15 * time.Minute).Milliseconds()); err != nil {
		t.Fatal(err)
	}
	h.DriveAll()
	if got := h.acq(b).State; got != storage.AcqBound {
		t.Fatalf("after crash mid-start: %s", got)
	}
	if h.CloudObjects() != 1 {
		t.Fatalf("crash inflated the fleet: %d machines", h.CloudObjects())
	}
}

// Explicit :park/:start semantics incl. the idempotent no-op and the
// capability gate (docs/12 §4).
func TestExplicitParkStart(t *testing.T) {
	h := newHarness(t, parkFake())
	ctx := context.Background()
	a := h.Acquire(app.AcquireCmd{Class: "ci-park", TTL: time.Minute})
	h.DriveAll()
	_ = h.Svc.Release(ctx, string(a), "test")
	list, _ := h.St.Resources().List(ctx, storage.ResourceFilter{Class: "ci-park"})
	id := string(list[0].ID)

	if err := h.Svc.ParkResource(ctx, id, "op"); err != nil {
		t.Fatal(err)
	}
	if err := h.Svc.ParkResource(ctx, id, "op"); !errors.Is(err, app.ErrAlreadyThere) {
		t.Fatalf("re-park while parking: %v, want ErrAlreadyThere", err)
	}
	h.DriveAll()
	if err := h.Svc.ParkResource(ctx, id, "op"); !errors.Is(err, app.ErrAlreadyThere) {
		t.Fatalf("park of parked: %v, want ErrAlreadyThere", err)
	}
	if err := h.Svc.StartResource(ctx, id, "op"); err != nil {
		t.Fatal(err)
	}
	h.DriveAll()
	if err := h.Svc.StartResource(ctx, id, "op"); !errors.Is(err, app.ErrAlreadyThere) {
		t.Fatalf("start of running: %v, want ErrAlreadyThere", err)
	}
	cur, _ := h.St.Resources().Get(ctx, storage.ResourceID(id))
	if cur.Phase != phase.Ready || cur.ParkedAt != nil {
		t.Fatalf("after explicit cycle: phase=%s parkedAt=%v", cur.Phase, cur.ParkedAt)
	}
}

// A provider without the capability never sees a stop action (docs/12
// invariant 6): the gate is inside the journaling transaction.
func TestParkUnsupportedProvider(t *testing.T) {
	h := newHarness(t, fake.Options{}) // no Park
	ctx := context.Background()
	a := h.Acquire(app.AcquireCmd{Class: "ci-park", TTL: time.Minute})
	h.DriveAll()
	_ = h.Svc.Release(ctx, string(a), "test")
	list, _ := h.St.Resources().List(ctx, storage.ResourceFilter{Class: "ci-park"})

	if err := h.Svc.ParkResource(ctx, string(list[0].ID), "op"); !errors.Is(err, provision.ErrParkUnsupported) {
		t.Fatalf("park on non-capable provider: %v, want ErrParkUnsupported", err)
	}
	// Stage 1 falls back to delete on non-capable providers.
	h.Clock.Advance(11 * time.Minute)
	if n, _ := h.Rec.ReclaimPoolless(ctx); n != 1 {
		t.Fatal("stage-1 delete fallback not journaled")
	}
	h.DriveAll()
	if h.CloudObjects() != 0 {
		t.Fatalf("fallback delete did not run: %d objects", h.CloudObjects())
	}
}
