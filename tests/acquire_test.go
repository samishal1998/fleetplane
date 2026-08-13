package tests

// Acquisition/lease/capacity tests (docs 04 §4/§6, 05 §3–5). Phase-5 exit:
// concurrent acquires cannot over-allocate (invariant 1); invariant 3 is
// pinned in both directions via TTL expiry and release.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/samimishal/fleetplane/internal/app"
	"github.com/samimishal/fleetplane/internal/capacity"
	"github.com/samimishal/fleetplane/internal/phase"
	"github.com/samimishal/fleetplane/internal/storage"
	"github.com/samimishal/fleetplane/providers/fake"
)

const cpu1 = `{"cpu":{"min":1}}`

// The demo's heart (08 §7): the second acquire REUSES existing capacity
// before creating another VM.
func TestAcquire_ReuseBeforeCreate(t *testing.T) {
	h := newHarness(t, fake.Options{})

	a1 := h.Acquire(app.AcquireCmd{Class: "ci", Constraints: json.RawMessage(cpu1)})
	if h.acq(a1).State != storage.AcqProvisioning {
		t.Fatalf("first acquire state = %s, want provisioning (no capacity yet)", h.acq(a1).State)
	}
	h.DriveAll() // create lands → OnTerminal binds
	if h.acq(a1).State != storage.AcqBound {
		t.Fatalf("first acquire not bound after create: %s", h.acq(a1).State)
	}

	// Second acquire: the fake machine has cpu:2 — one cpu is free.
	a2 := h.Acquire(app.AcquireCmd{Class: "ci", Constraints: json.RawMessage(cpu1)})
	if got := h.acq(a2); got.State != storage.AcqBound {
		t.Fatalf("second acquire = %s, want bound to EXISTING capacity", got.State)
	}
	if n := h.CloudObjects(); n != 1 {
		t.Fatalf("cloud has %d machines, want 1 — reuse before create (08 §7)", n)
	}

	// Third acquire: capacity exhausted → a second machine.
	a3 := h.Acquire(app.AcquireCmd{Class: "ci", Constraints: json.RawMessage(cpu1)})
	h.DriveAll()
	if h.acq(a3).State != storage.AcqBound {
		t.Fatalf("third acquire: %s", h.acq(a3).State)
	}
	if n := h.CloudObjects(); n != 2 {
		t.Fatalf("cloud has %d machines, want 2", n)
	}
}

// No constraints = whole-machine semantics: exclusive lease, no sharing.
func TestAcquire_ExclusiveWholeMachine(t *testing.T) {
	h := newHarness(t, fake.Options{})
	a1 := h.Acquire(app.AcquireCmd{Class: "ci"})
	h.DriveAll()
	a2 := h.Acquire(app.AcquireCmd{Class: "ci"})
	h.DriveAll()
	if h.acq(a1).State != storage.AcqBound || h.acq(a2).State != storage.AcqBound {
		t.Fatalf("states: %s %s", h.acq(a1).State, h.acq(a2).State)
	}
	if n := h.CloudObjects(); n != 2 {
		t.Fatalf("exclusive acquires shared a machine: %d objects", n)
	}
}

// Invariant 1 + Phase-5 exit: hammer one 2-cpu machine with concurrent
// 1-cpu acquires (classless = no scale-up); exactly 2 bind and the active
// lease sum never exceeds capacity.
func TestConcurrentAcquire_NoOverAllocation(t *testing.T) {
	h := newHarness(t, fake.Options{})
	seed := h.Acquire(app.AcquireCmd{Class: "ci", Constraints: json.RawMessage(cpu1)})
	h.DriveAll()
	res := h.acq(seed)
	if res.State != storage.AcqBound {
		t.Fatal("seed acquire failed")
	}
	// Free the seed lease so the full 2 cpu are available.
	if err := h.Svc.Release(context.Background(), string(seed), "test"); err != nil {
		t.Fatal(err)
	}

	const workers = 8
	var wg sync.WaitGroup
	states := make([]storage.AcqState, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := h.Acquire(app.AcquireCmd{ // classless: reuse only
				Kind: "compute.machine", Constraints: json.RawMessage(cpu1),
			})
			states[i] = h.acq(id).State
		}(i)
	}
	wg.Wait()

	bound := 0
	for _, s := range states {
		if s == storage.AcqBound {
			bound++
		}
	}
	if bound != 2 {
		t.Fatalf("%d concurrent acquires bound on a 2-cpu machine, want 2 (invariant 1)", bound)
	}
	// The DB-level truth: Σ active leases ≤ capacity.
	list, _ := h.St.Resources().List(context.Background(), storage.ResourceFilter{Kind: "compute.machine"})
	for _, r := range list {
		sum, err := h.St.Leases().SumActive(context.Background(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		total, _ := capacity.Parse(r.Capacity)
		for dim, used := range sum {
			if used > total[dim] {
				t.Fatalf("resource %s over-allocated: %s used %d of %d", r.ID, dim, used, total[dim])
			}
		}
	}
}

// Invariant 3 in both directions: an active lease blocks reclaim; expiry
// ends the lease, frees the phase and the acquisition.
func TestAcquire_TTLExpiry(t *testing.T) {
	h := newHarness(t, fake.Options{})
	a := h.Acquire(app.AcquireCmd{Class: "ci", Constraints: json.RawMessage(cpu1), TTL: time.Minute})
	h.DriveAll()
	acq := h.acq(a)
	if acq.State != storage.AcqBound {
		t.Fatal("not bound")
	}
	lease, err := h.St.Leases().Get(context.Background(), *acq.LeaseID)
	if err != nil {
		t.Fatal(err)
	}

	// Before expiry the sweep does nothing (invariant-3 direction 1).
	if n, _ := h.Leases.SweepExpired(context.Background()); n != 0 {
		t.Fatal("sweep expired an unexpired lease")
	}

	h.Clock.Advance(2 * time.Minute)
	n, err := h.Leases.SweepExpired(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	if got := h.acq(a); got.State != storage.AcqExpired {
		t.Fatalf("acquisition after expiry: %s", got.State)
	}
	res, _ := h.St.Resources().Get(context.Background(), lease.ResourceID)
	if res.Phase != phase.Ready || res.LastLeaseEndedAt == nil {
		t.Fatalf("resource after expiry: phase=%s lastLeaseEnded=%v", res.Phase, res.LastLeaseEndedAt)
	}
}

func TestAcquire_ReleaseThenReuse(t *testing.T) {
	h := newHarness(t, fake.Options{})
	a1 := h.Acquire(app.AcquireCmd{Class: "ci"})
	h.DriveAll()
	if err := h.Svc.Release(context.Background(), string(a1), "test"); err != nil {
		t.Fatal(err)
	}
	// Release is retry-safe.
	if err := h.Svc.Release(context.Background(), string(a1), "test"); err != nil {
		t.Fatalf("second release errored: %v", err)
	}

	a2 := h.Acquire(app.AcquireCmd{Class: "ci"})
	if got := h.acq(a2); got.State != storage.AcqBound {
		t.Fatalf("released machine not reused: %s", got.State)
	}
	if n := h.CloudObjects(); n != 1 {
		t.Fatalf("cloud has %d, want 1", n)
	}
}

// Plan R6: crash between the create landing and the bind — the resume scan
// completes the bind.
func TestAcquire_BindResumeAfterCrash(t *testing.T) {
	h := newHarness(t, fake.Options{})
	a := h.Acquire(app.AcquireCmd{Class: "ci", Constraints: json.RawMessage(cpu1)})

	// Drive the create WITHOUT the OnTerminal bind trigger (the crash
	// window between op-terminal commit and bind).
	h.Engine.OnTerminal = nil
	h.Drive(func() storage.ResourceID { return *h.acq(a).PendingResourceID }())
	if got := h.acq(a); got.State != storage.AcqProvisioning {
		t.Fatalf("precondition: %s, want provisioning", got.State)
	}

	h.Crash()
	h.Restart()
	if err := h.Sched.Resume(context.Background(), int64(15*time.Minute/time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if got := h.acq(a); got.State != storage.AcqBound {
		t.Fatalf("resume did not bind: %s (plan R6)", got.State)
	}
}

func TestAcquire_PendingTimeoutExpires(t *testing.T) {
	h := newHarness(t, fake.Options{})
	a := h.Acquire(app.AcquireCmd{Class: "ci", Constraints: json.RawMessage(cpu1)})
	// Never drive the engine; the create hangs forever.
	h.Clock.Advance(20 * time.Minute)
	if err := h.Sched.Resume(context.Background(), (15 * time.Minute).Milliseconds()); err != nil {
		t.Fatal(err)
	}
	if got := h.acq(a); got.State != storage.AcqExpired {
		t.Fatalf("stuck acquisition not expired: %s", got.State)
	}
}

func TestAcquire_IdempotentReplay(t *testing.T) {
	h := newHarness(t, fake.Options{})
	cmd := app.AcquireCmd{
		Class: "ci", Constraints: json.RawMessage(cpu1),
		Actor: "ci", IdemKey: "workflow-1", IdemScope: "acq|ci", RequestHash: "h",
		BuildResponse: func(a *storage.Acquisition) (int, json.RawMessage) {
			b, _ := json.Marshal(map[string]string{"id": string(a.ID)})
			return 201, b
		},
	}
	out1, err := h.Svc.Acquire(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := h.Svc.Acquire(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !out2.Replayed || string(out1.Body) != string(out2.Body) {
		t.Fatalf("replay mismatch: %v", out2)
	}
	open, _ := h.St.Acquisitions().ListByState(context.Background(),
		storage.AcqPending, storage.AcqProvisioning, storage.AcqBound)
	if len(open) != 1 {
		t.Fatalf("replayed acquire produced %d acquisitions, want 1 (invariant 2)", len(open))
	}
}

// Plan R21 + demo step 9: released poolless capacity is idle-reclaimed by
// CLASS policy — the machine is eventually deleted with no pool involved.
func TestAcquire_PoollessIdleReclaim(t *testing.T) {
	h := newHarness(t, fake.Options{})
	a := h.Acquire(app.AcquireCmd{Class: "ci-reclaim"})
	h.DriveAll()
	if err := h.Svc.Release(context.Background(), string(a), "test"); err != nil {
		t.Fatal(err)
	}

	// Not yet idle long enough.
	if n, err := h.Rec.ReclaimPoolless(context.Background()); err != nil || n != 0 {
		t.Fatalf("reclaimed before idleAfter: %d %v", n, err)
	}
	h.Clock.Advance(11 * time.Minute)
	if n, err := h.Rec.ReclaimPoolless(context.Background()); err != nil || n != 1 {
		t.Fatalf("reclaim = %d %v, want 1", n, err)
	}
	h.DriveAll()
	if n := h.CloudObjects(); n != 0 {
		t.Fatalf("idle machine not deleted: %d objects", n)
	}
}
