package tests

// Phase 9 (doc 09): a deliberately non-VM kind — storage.volume — flows
// through the UNCHANGED kernel: journal, pools, reconciliation, acquisition
// scheduling, capacity math and deletion all work with no kernel edits.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/samishal1998/fleetplane/internal/app"
	"github.com/samishal1998/fleetplane/internal/phase"
	"github.com/samishal1998/fleetplane/internal/reconcile"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/pkg/kinds"
	"github.com/samishal1998/fleetplane/pkg/kinds/volume"
	"github.com/samishal1998/fleetplane/providers/fake"
)

func TestVolume_KindRegistered(t *testing.T) {
	d, ok := kinds.Get(volume.Kind)
	if !ok {
		t.Fatal("storage.volume not registered")
	}
	if err := d.ValidateSpec(json.RawMessage(`{"sizeGiB":100}`)); err != nil {
		t.Fatal(err)
	}
	if err := d.ValidateSpec(json.RawMessage(`{"sizeGiB":0}`)); err == nil {
		t.Fatal("zero-size volume accepted")
	}
	// The kernel rejects unregistered kinds with the registry's message.
	if err := kinds.Validate("database.postgres", nil); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestVolume_CreateThroughUnchangedKernel(t *testing.T) {
	h := newHarness(t, fake.Options{})
	out, err := h.Svc.CreateResource(context.Background(), app.CreateResourceCmd{
		Kind: "storage.volume", Provider: "fake-local", Name: "data-1",
		Spec: json.RawMessage(`{"sizeGiB":250,"filesystem":"xfs"}`), Actor: "test",
		BuildResponse: func(r *storage.Resource) (int, json.RawMessage) {
			b, _ := json.Marshal(map[string]string{"id": string(r.ID)})
			return 201, b
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ ID string }
	_ = json.Unmarshal(out.Body, &body)
	resID := storage.ResourceID(body.ID)
	h.Drive(resID)

	res, err := h.St.Resources().Get(context.Background(), resID)
	if err != nil || res.Phase != phase.Ready {
		t.Fatalf("volume: %+v %v", res, err)
	}
	var cap map[string]int64
	_ = json.Unmarshal(res.Capacity, &cap)
	if cap["storageGiB"] != 250 {
		t.Fatalf("capacity = %v, want storageGiB 250", cap)
	}
}

func TestVolume_PoolConverges(t *testing.T) {
	h := newHarness(t, fake.Options{})
	pool := h.CreatePool("volumes", reconcile.PoolSpec{Class: "vol-100", Replicas: 3})
	h.Converge(pool)
	if got := h.PoolPhases(pool); got["ready"] != 3 {
		t.Fatalf("volume pool: %v, want 3 ready — reconciliation is VM-shaped", got)
	}
	// Scale down uses the same drain→delete pipeline.
	h.UpdatePool(pool, "volumes", reconcile.PoolSpec{Class: "vol-100", Replicas: 1})
	h.Converge(pool)
	if got := h.PoolPhases(pool); got["ready"] != 1 {
		t.Fatalf("volume scale-down: %v", got)
	}
}

func TestVolume_AcquireByStorageConstraint(t *testing.T) {
	h := newHarness(t, fake.Options{})
	a1 := h.Acquire(app.AcquireCmd{
		Kind: "storage.volume", Class: "vol-100",
		Constraints: json.RawMessage(`{"storageGiB":{"min":40}}`),
	})
	h.DriveAll()
	if got := h.acq(a1); got.State != storage.AcqBound {
		t.Fatalf("volume acquire: %s", got.State)
	}
	// A second 40 GiB slice fits on the same 100 GiB volume: shared
	// capacity scheduling is dimension-agnostic.
	a2 := h.Acquire(app.AcquireCmd{
		Kind: "storage.volume", Class: "vol-100",
		Constraints: json.RawMessage(`{"storageGiB":{"min":40}}`),
	})
	if got := h.acq(a2); got.State != storage.AcqBound {
		t.Fatalf("second slice: %s", got.State)
	}
	// A third doesn't fit (40+40+40 > 100) → new volume provisioned.
	a3 := h.Acquire(app.AcquireCmd{
		Kind: "storage.volume", Class: "vol-100",
		Constraints: json.RawMessage(`{"storageGiB":{"min":40}}`),
	})
	h.DriveAll()
	if got := h.acq(a3); got.State != storage.AcqBound {
		t.Fatalf("third slice: %s", got.State)
	}
	vols, _ := h.St.Resources().List(context.Background(), storage.ResourceFilter{Kind: "storage.volume"})
	if len(vols) != 2 {
		t.Fatalf("%d volumes, want 2 (reuse before create, kind-agnostic)", len(vols))
	}
}
