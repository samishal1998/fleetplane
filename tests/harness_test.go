package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/samishal1998/fleetplane/internal/app"
	"github.com/samishal1998/fleetplane/internal/ids"
	"github.com/samishal1998/fleetplane/internal/lease"
	"github.com/samishal1998/fleetplane/internal/operations"
	"github.com/samishal1998/fleetplane/internal/reconcile"
	"github.com/samishal1998/fleetplane/internal/scheduler"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/internal/storage/sqlite"
	"github.com/samishal1998/fleetplane/pkg/kinds/compute"
	"github.com/samishal1998/fleetplane/pkg/sdk/secretref"
	"github.com/samishal1998/fleetplane/providers/fake"
)

// stepClock is a controllable clock: tests Advance it so persisted backoff
// deadlines come due without sleeping.
type stepClock struct {
	mu   sync.Mutex
	base time.Time
	skew time.Duration
}

func newStepClock() *stepClock { return &stepClock{base: time.Now()} }

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.base.Add(c.skew)
}

func (c *stepClock) After(time.Duration) <-chan time.Time { return time.After(5 * time.Millisecond) }

func (c *stepClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.skew += d
}

// Harness assembles the stack over a DB file. Crash() abandons the process
// state (hard-closes the store, no shutdown hooks); Restart() reopens over
// the same file and runs Resume. The fake provider instance survives across
// restarts — the cloud outlives the control plane.
type Harness struct {
	t      *testing.T
	dbPath string

	St        storage.Store
	Providers *app.Providers
	Fake      *fake.Fake
	Engine    *operations.Engine
	Rec       *reconcile.Reconciler
	Sched     *scheduler.Scheduler
	Leases    *lease.Manager
	Svc       *app.Service
	Clock     *stepClock
	Hooks     operations.Hooks
	EngineCfg operations.Config

	ownerID string
}

func (h *Harness) OwnerID() string { return h.ownerID }

func newHarness(t *testing.T, fakeOpts fake.Options) *Harness {
	t.Helper()
	h := &Harness{
		t:      t,
		dbPath: filepath.Join(t.TempDir(), "fp.db"),
		Clock:  newStepClock(),
	}
	h.open(nil, fakeOpts)
	return h
}

func (h *Harness) open(providers *app.Providers, fakeOpts fake.Options) {
	h.t.Helper()
	ctx := context.Background()
	st, err := sqlite.OpenStore(ctx, h.dbPath)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ownerID, err := app.EnsureOwnerID(ctx, st, h.Clock.Now().UnixMilli())
	if err != nil {
		h.t.Fatal(err)
	}
	if providers == nil {
		settings, _ := json.Marshal(fakeOpts)
		providers, err = app.BuildProviders(ctx, st,
			[]app.ProviderSpec{{Name: "fake-local", Driver: "fake", Settings: settings}},
			ownerID, secretref.NewDefault(), log, h.Clock.Now().UnixMilli())
		if err != nil {
			h.t.Fatal(err)
		}
	}
	inst, _ := providers.Instance("fake-local")
	h.St = st
	h.ownerID = ownerID
	h.Providers = providers
	h.Fake = inst.(*fake.Fake)
	h.Engine = operations.New(st, providers, h.Clock, log, h.EngineCfg, h.Hooks)
	classes := app.Classes{
		"ci": reconcile.Class{
			Kind: "compute.machine", Provider: "fake-local",
			Spec: json.RawMessage(machineSpec),
		},
		"ci-reclaim": reconcile.Class{
			Kind: "compute.machine", Provider: "fake-local",
			Spec:    json.RawMessage(machineSpec),
			Reclaim: &reconcile.ReclaimPolicy{IdleAfter: compute.Duration(10 * time.Minute)},
		},
		"vol-100": reconcile.Class{
			Kind: "storage.volume", Provider: "fake-local",
			Spec: json.RawMessage(`{"sizeGiB":100,"filesystem":"ext4"}`),
		},
		// docs/11: queue-enabled class — acquisitions wait up to 10m for
		// existing/in-flight capacity before scaling up.
		"ci-queue": reconcile.Class{
			Kind: "compute.machine", Provider: "fake-local",
			Spec:         json.RawMessage(machineSpec),
			Reclaim:      &reconcile.ReclaimPolicy{IdleAfter: compute.Duration(10 * time.Minute)},
			QueueMaxWait: 10 * time.Minute,
		},
	}
	// Production parity: classes are seeded into storage and resolved
	// through the store-backed registry (dynamic classes).
	if err := app.SeedConfigClasses(ctx, st, classes, h.Clock.Now().UnixMilli()); err != nil {
		h.t.Fatal(err)
	}
	classReg := app.NewClassRegistry(st, log)
	h.Rec = reconcile.New(st, providers, h.Engine, classReg, h.Clock, log, ownerID, reconcile.Config{})
	h.Sched = scheduler.New(st, providers, h.Engine, classReg, h.Clock, log, ownerID)
	h.Leases = lease.New(st, h.Clock, log, h.Rec)
	h.Engine.OnTerminal = func(op *storage.Operation) {
		h.Rec.HandleOpTerminal(op)
		h.Sched.HandleOpTerminal(op)
	}
	h.Svc = app.NewService(st, providers, h.Engine, h.Clock, log, ownerID)
	h.Svc.AttachScheduling(h.Sched, h.Leases)
}

// Acquire submits an acquisition through the service and returns its ID.
func (h *Harness) Acquire(cmd app.AcquireCmd) storage.AcquisitionID {
	h.t.Helper()
	if cmd.Actor == "" {
		cmd.Actor = "test"
	}
	if cmd.BuildResponse == nil {
		cmd.BuildResponse = func(a *storage.Acquisition) (int, json.RawMessage) {
			b, _ := json.Marshal(map[string]string{"id": string(a.ID), "state": string(a.State)})
			return 201, b
		}
	}
	out, err := h.Svc.Acquire(context.Background(), cmd)
	if err != nil {
		h.t.Fatal(err)
	}
	var body struct{ ID string }
	if err := json.Unmarshal(out.Body, &body); err != nil {
		h.t.Fatal(err)
	}
	return storage.AcquisitionID(body.ID)
}

func (h *Harness) acq(id storage.AcquisitionID) *storage.Acquisition {
	h.t.Helper()
	a, err := h.St.Acquisitions().Get(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return a
}

// CreatePool upserts a pool spec and returns its ID.
func (h *Harness) CreatePool(name string, spec reconcile.PoolSpec) storage.PoolID {
	h.t.Helper()
	raw, _ := json.Marshal(spec)
	id := storage.PoolID(ids.New(ids.Pool))
	err := h.St.Tx(context.Background(), func(tx storage.TxStore) error {
		return tx.Pools().Upsert(context.Background(), &storage.Pool{
			ID: id, Name: name, Kind: "compute.machine", Spec: raw,
			Generation: 1, CreatedAt: h.Clock.Now().UnixMilli(), UpdatedAt: h.Clock.Now().UnixMilli(),
		})
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

// UpdatePool replaces a pool's spec.
func (h *Harness) UpdatePool(id storage.PoolID, name string, spec reconcile.PoolSpec) {
	h.t.Helper()
	raw, _ := json.Marshal(spec)
	err := h.St.Tx(context.Background(), func(tx storage.TxStore) error {
		return tx.Pools().Upsert(context.Background(), &storage.Pool{
			ID: id, Name: name, Kind: "compute.machine", Spec: raw,
			UpdatedAt: h.Clock.Now().UnixMilli(),
		})
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

// Converge alternates reconcile cycles with engine drives until the pool is
// stable (no mutations decided, no open ops) or the bound trips.
func (h *Harness) Converge(poolID storage.PoolID) reconcile.Delta {
	h.t.Helper()
	ctx := context.Background()
	var last reconcile.Delta
	for i := 0; i < 100; i++ {
		d, err := h.Rec.RunOnce(ctx, poolID)
		if err != nil {
			h.t.Fatal(err)
		}
		last = d
		h.DriveAll()
		if d.Created == 0 && d.Drained == 0 && d.Undrained == 0 && d.Deleted == 0 {
			open, err := h.St.Operations().NonTerminal(ctx)
			if err != nil {
				h.t.Fatal(err)
			}
			if len(open) == 0 {
				return last
			}
		}
		h.Clock.Advance(7 * time.Second)
	}
	h.t.Fatal("pool never converged")
	return last
}

// DriveAll steps the engine until no operations remain non-terminal or
// progress stalls past the bound.
func (h *Harness) DriveAll() {
	h.t.Helper()
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		if _, err := h.Engine.Step(ctx); err != nil {
			h.t.Fatal(err)
		}
		open, err := h.St.Operations().NonTerminal(ctx)
		if err != nil {
			h.t.Fatal(err)
		}
		if len(open) == 0 {
			return
		}
		h.Clock.Advance(7 * time.Second)
	}
	h.t.Fatal("operations never drained")
}

// PoolPhases returns pool member counts by phase (tombstoned excluded).
func (h *Harness) PoolPhases(poolID storage.PoolID) map[string]int {
	h.t.Helper()
	list, err := h.St.Resources().List(context.Background(), storage.ResourceFilter{PoolID: &poolID})
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]int{}
	for _, r := range list {
		out[string(r.Phase)]++
	}
	return out
}

// Crash hard-stops the "process": the store closes without any shutdown
// niceties; engine/service references become garbage.
func (h *Harness) Crash() { _ = h.St.Close() }

// Restart models a process restart over the same database and the same
// cloud (fake instance); it runs Engine.Resume like boot does (plan R11).
func (h *Harness) Restart() {
	h.t.Helper()
	h.open(h.Providers, fake.Options{})
	if err := h.Engine.Resume(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

// Create journals a create through the service (TxA) without driving it.
func (h *Harness) Create(name string) storage.ResourceID {
	h.t.Helper()
	out, err := h.Svc.CreateResource(context.Background(), app.CreateResourceCmd{
		Kind: "compute.machine", Provider: "fake-local", Name: name,
		Spec: json.RawMessage(machineSpec), Actor: "test",
		BuildResponse: func(r *storage.Resource) (int, json.RawMessage) {
			b, _ := json.Marshal(map[string]string{"id": string(r.ID)})
			return 201, b
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	var body struct{ ID string }
	if err := json.Unmarshal(out.Body, &body); err != nil {
		h.t.Fatal(err)
	}
	return storage.ResourceID(body.ID)
}

// Drive steps the engine (advancing the clock between steps so backoff
// deadlines come due) until the resource has no open operations.
func (h *Harness) Drive(resID storage.ResourceID) {
	h.t.Helper()
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		if _, err := h.Engine.Step(ctx); err != nil {
			h.t.Fatal(err)
		}
		open, err := h.openOps(resID)
		if err != nil {
			h.t.Fatal(err)
		}
		if open == 0 {
			return
		}
		h.Clock.Advance(7 * time.Second)
	}
	h.t.Fatal("operation never reached a terminal state")
}

func (h *Harness) openOps(resID storage.ResourceID) (int, error) {
	ops, err := h.St.Operations().NonTerminal(context.Background())
	if err != nil {
		return 0, err
	}
	n := 0
	for _, op := range ops {
		if op.ResourceID != nil && *op.ResourceID == resID {
			n++
		}
	}
	return n, nil
}

func (h *Harness) opFor(resID storage.ResourceID) (*storage.Operation, error) {
	ops, err := h.St.Operations().NonTerminal(context.Background())
	if err != nil {
		return nil, err
	}
	for _, op := range ops {
		if op.ResourceID != nil && *op.ResourceID == resID {
			return op, nil
		}
	}
	return nil, fmt.Errorf("no open operation for %s", resID)
}

// CloudObjects counts non-gone provider-side objects — the invariant-7
// ground truth.
func (h *Harness) CloudObjects() int { return len(h.Fake.Objects()) }
