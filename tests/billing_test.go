package tests

// docs/11 proofs: billing-window-delayed reclaim, sequential lease packing
// via queued acquisitions, deadline force-scale, queued-work protection,
// crash-resume of the queue decision, and billing-aware placement.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/samishal1998/fleetplane/internal/app"
	"github.com/samishal1998/fleetplane/internal/capacity"
	"github.com/samishal1998/fleetplane/internal/ids"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/pkg/kinds/compute"
	"github.com/samishal1998/fleetplane/providers/fake"
)

func hourlyFake() fake.Options {
	return fake.Options{Billing: map[string]fake.BillingSettings{
		"compute.machine": {
			Increment:         compute.Duration(time.Hour),
			TerminationBuffer: compute.Duration(6 * time.Minute),
		},
	}}
}

// idleMachine acquires+releases one ci-reclaim machine and returns it.
func idleMachine(t *testing.T, h *Harness) *storage.Resource {
	t.Helper()
	a := h.Acquire(app.AcquireCmd{Class: "ci-reclaim", TTL: time.Minute})
	h.DriveAll()
	if got := h.acq(a).State; got != storage.AcqBound {
		t.Fatalf("seed acquisition state = %s", got)
	}
	if err := h.Svc.Release(context.Background(), string(a), "test"); err != nil {
		t.Fatal(err)
	}
	list, err := h.St.Resources().List(context.Background(), storage.ResourceFilter{Class: "ci-reclaim"})
	if err != nil || len(list) != 1 {
		t.Fatalf("resources = %d (%v)", len(list), err)
	}
	return list[0]
}

func advanceTo(h *Harness, atMs int64) {
	if d := atMs - h.Clock.Now().UnixMilli(); d > 0 {
		h.Clock.Advance(time.Duration(d) * time.Millisecond)
	}
}

// Billing-aware idle reclaim waits for the termination window: eligible by
// idle policy long before, deleted only inside [boundary-buffer,
// boundary-cutoff] (docs/11 §11, §13).
func TestBillingWindowDelaysReclaim(t *testing.T) {
	h := newHarness(t, hourlyFake())
	res := idleMachine(t, h)
	hourMs := time.Hour.Milliseconds()

	// Idle-eligible (idleAfter 10m) but well before the window: kept.
	advanceTo(h, res.CreatedAt+20*60_000)
	if n, _ := h.Rec.ReclaimPoolless(context.Background()); n != 0 {
		t.Fatalf("reclaimed %d before the termination window", n)
	}
	// Inside the window (buffer 6m, cutoff 3m → [54m, 57m]): deleted.
	advanceTo(h, res.CreatedAt+hourMs-5*60_000)
	if n, _ := h.Rec.ReclaimPoolless(context.Background()); n != 1 {
		t.Fatalf("expected the window delete, got %d", n)
	}
	h.DriveAll()
	if h.CloudObjects() != 0 {
		t.Fatalf("cloud objects = %d after window delete", h.CloudObjects())
	}
}

// Past the cutoff the increment is inescapable: keep the machine and target
// the NEXT boundary — crossing is a decision, not an accident (docs/11 §14).
func TestBillingWindowMissedTargetsNextBoundary(t *testing.T) {
	h := newHarness(t, hourlyFake())
	res := idleMachine(t, h)
	hourMs := time.Hour.Milliseconds()

	advanceTo(h, res.CreatedAt+hourMs-60_000) // past cutoff (57m)
	if n, _ := h.Rec.ReclaimPoolless(context.Background()); n != 0 {
		t.Fatalf("deleted inside the point-of-no-return, %d", n)
	}
	// Next window: [1h54m, 1h57m].
	advanceTo(h, res.CreatedAt+2*hourMs-5*60_000)
	if n, _ := h.Rec.ReclaimPoolless(context.Background()); n != 1 {
		t.Fatalf("expected delete in the NEXT window, got %d", n)
	}
}

// The §7 headline: two near-simultaneous requests pack into ONE machine —
// the second queues behind the first's in-flight provision instead of
// scaling, then binds after release.
func TestQueuePacksSequentialLeases(t *testing.T) {
	opts := hourlyFake()
	opts.CreateSteps = 2 // async create: B arrives while A's machine is in flight
	h := newHarness(t, opts)
	ctx := context.Background()

	a := h.Acquire(app.AcquireCmd{Class: "ci-queue", TTL: 5 * time.Minute})
	b := h.Acquire(app.AcquireCmd{Class: "ci-queue", TTL: 5 * time.Minute})

	if got := h.acq(a).State; got != storage.AcqProvisioning {
		t.Fatalf("A state = %s", got)
	}
	if got := h.acq(b).State; got != storage.AcqPending {
		t.Fatalf("B must queue behind A's in-flight machine, state = %s", got)
	}
	h.DriveAll() // A's create lands; A binds via OnTerminal
	if got := h.acq(a).State; got != storage.AcqBound {
		t.Fatalf("A state after drive = %s", got)
	}
	if got := h.acq(b).State; got != storage.AcqPending {
		t.Fatalf("B state after drive = %s", got)
	}
	if err := h.Svc.Release(ctx, string(a), "test"); err != nil {
		t.Fatal(err)
	}
	if err := h.Sched.Resume(ctx, (15 * time.Minute).Milliseconds()); err != nil {
		t.Fatal(err)
	}
	bAcq := h.acq(b)
	if bAcq.State != storage.AcqBound {
		t.Fatalf("B state after release+sweep = %s", bAcq.State)
	}
	if h.CloudObjects() != 1 {
		t.Fatalf("packing failed: %d machines for 2 sequential requests", h.CloudObjects())
	}
}

// A queued acquisition whose deadline passes without capacity force-scales
// (never silently waits for pendingTimeout — docs/11 invariant 4).
func TestQueueDeadlineForceScales(t *testing.T) {
	opts := hourlyFake()
	opts.CreateSteps = 2
	h := newHarness(t, opts)
	ctx := context.Background()

	a := h.Acquire(app.AcquireCmd{Class: "ci-queue", TTL: 8 * time.Minute})
	b := h.Acquire(app.AcquireCmd{Class: "ci-queue", TTL: 8 * time.Minute})
	if got := h.acq(b).State; got != storage.AcqPending {
		t.Fatalf("B state = %s", got)
	}
	// B's 10m queue budget elapses while A's create is still in flight.
	h.Clock.Advance(11 * time.Minute)
	if err := h.Sched.Resume(ctx, (15 * time.Minute).Milliseconds()); err != nil {
		t.Fatal(err)
	}
	if got := h.acq(b).State; got != storage.AcqProvisioning {
		t.Fatalf("B must force-scale at its deadline, state = %s", got)
	}
	h.DriveAll()
	if h.acq(a).State != storage.AcqBound || h.acq(b).State != storage.AcqBound {
		t.Fatalf("states after drive: A=%s B=%s", h.acq(a).State, h.acq(b).State)
	}
	if h.CloudObjects() != 2 {
		t.Fatalf("expected 2 machines after force-scale, got %d", h.CloudObjects())
	}
}

// A pending compatible acquisition blocks cost-based termination of the
// machine it could bind to (docs/11 invariant 6) — checked inside the
// delete transaction, so even a race can't delete it.
func TestQueuedAcquisitionBlocksReclaim(t *testing.T) {
	h := newHarness(t, hourlyFake())
	ctx := context.Background()
	res := idleMachine(t, h)
	hourMs := time.Hour.Milliseconds()
	advanceTo(h, res.CreatedAt+hourMs-5*60_000) // inside the window

	// A queued acquisition materializing between sweep decision and delete
	// (simulated by direct insert, the worst-case ordering).
	nowMs := h.Clock.Now().UnixMilli()
	acqID := storage.AcquisitionID(ids.New(ids.Acquisition))
	req, _ := json.Marshal(capacity.Request{MaxWaitMs: (30 * time.Minute).Milliseconds()})
	if err := h.St.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Acquisitions().Insert(ctx, &storage.Acquisition{
			ID: acqID, Actor: "test", Kind: "compute.machine", Class: "ci-reclaim",
			Constraints: req, Quantity: 1, State: storage.AcqPending,
			CreatedAt: nowMs, UpdatedAt: nowMs,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := h.Rec.ReclaimPoolless(ctx); n != 0 {
		t.Fatalf("deleted the machine a queued acquisition waits for (%d)", n)
	}
	// Withdraw the queued request: the machine is reclaimable again.
	if err := h.St.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Acquisitions().Transition(ctx, acqID, storage.AcqPending, storage.AcqReleased, h.Clock.Now().UnixMilli())
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := h.Rec.ReclaimPoolless(ctx); n != 1 {
		t.Fatalf("expected reclaim after withdrawal, got %d", n)
	}
	if h.acq(acqID).State != storage.AcqReleased {
		t.Fatal("acquisition state changed unexpectedly")
	}
}

// kill -9 while queued: the persisted maxWait re-queues B through the same
// decision after restart — no phantom scale-up, same deadline.
func TestQueueSurvivesCrash(t *testing.T) {
	opts := hourlyFake()
	opts.CreateSteps = 2
	h := newHarness(t, opts)
	ctx := context.Background()

	a := h.Acquire(app.AcquireCmd{Class: "ci-queue", TTL: 8 * time.Minute})
	b := h.Acquire(app.AcquireCmd{Class: "ci-queue", TTL: 8 * time.Minute})
	if got := h.acq(b).State; got != storage.AcqPending {
		t.Fatalf("B state = %s", got)
	}

	h.Crash()
	h.Restart()

	if err := h.Sched.Resume(ctx, (15 * time.Minute).Milliseconds()); err != nil {
		t.Fatal(err)
	}
	if got := h.acq(b).State; got != storage.AcqPending {
		t.Fatalf("B must still be queued after restart, state = %s", got)
	}
	h.DriveAll()
	if err := h.Svc.Release(ctx, string(a), "test"); err != nil {
		t.Fatal(err)
	}
	if err := h.Sched.Resume(ctx, (15 * time.Minute).Milliseconds()); err != nil {
		t.Fatal(err)
	}
	if got := h.acq(b).State; got != storage.AcqBound {
		t.Fatalf("B after release = %s", got)
	}
	if h.CloudObjects() != 1 {
		t.Fatalf("crash inflated the fleet: %d machines", h.CloudObjects())
	}
}

// Billing-aware placement (docs/11 §9): a lease that fits inside one
// machine's remaining SAFE window (before boundary - buffer) prefers that
// machine over one it would push past its reclaim point.
func TestBillingAwarePlacement(t *testing.T) {
	h := newHarness(t, hourlyFake())
	ctx := context.Background()

	// m1 at t0, held while m2 is created ~28m later; then both released.
	a1 := h.Acquire(app.AcquireCmd{Class: "ci-reclaim"})
	h.DriveAll()
	h.Clock.Advance(28 * time.Minute)
	a2 := h.Acquire(app.AcquireCmd{Class: "ci-reclaim"})
	h.DriveAll()
	list, _ := h.St.Resources().List(ctx, storage.ResourceFilter{Class: "ci-reclaim"})
	if len(list) != 2 {
		t.Fatalf("resources = %d", len(list))
	}
	var m1, m2 *storage.Resource
	for _, r := range list {
		if m1 == nil || r.CreatedAt < m1.CreatedAt {
			m1 = r
		}
	}
	for _, r := range list {
		if r.ID != m1.ID {
			m2 = r
		}
	}
	_ = h.Svc.Release(ctx, string(a1), "test")
	_ = h.Svc.Release(ctx, string(a2), "test")

	// At m1+48m: a 10m lease ends at +58m — inside m1's buffer (safe point
	// 54m) but comfortably before m2's (safe point ~82m): m2 must win even
	// though plain best-fit ULID order would pick m1.
	advanceTo(h, m1.CreatedAt+48*60_000)
	a3 := h.Acquire(app.AcquireCmd{Class: "ci-reclaim", TTL: 10 * time.Minute})
	acq := h.acq(a3)
	if acq.State != storage.AcqBound || acq.LeaseID == nil {
		t.Fatalf("a3 = %s", acq.State)
	}
	l, err := h.St.Leases().Get(ctx, *acq.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if l.ResourceID != m2.ID {
		t.Fatalf("placed on %s (would defer its reclaim); want %s (fits its paid window)", l.ResourceID, m2.ID)
	}
}
