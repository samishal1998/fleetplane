package tests

// Ownership semantics (docs 07 §5, ADR-017, plan R8/R9): orphan gates,
// ghost disposition, restore re-adoption (the 06 §8 metric), adoption of
// foreign resources as observed, and deletion safeguards.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/samimishal/fleetplane/internal/app"
	"github.com/samimishal/fleetplane/internal/phase"
	"github.com/samimishal/fleetplane/internal/reconcile"
	"github.com/samimishal/fleetplane/internal/storage"
	"github.com/samimishal/fleetplane/pkg/sdk/provider"
	"github.com/samimishal/fleetplane/providers/fake"
)

func enableDiscovery(h *Harness, cfg reconcile.DiscoveryConfig, healthy func(string) bool) {
	h.Rec.EnableDiscovery(cfg, func() []string { return []string{"fake-local"} }, healthy)
}

func sweep(t *testing.T, h *Harness) {
	t.Helper()
	if err := h.Rec.SweepProvider(context.Background(), "fake-local"); err != nil {
		t.Fatal(err)
	}
}

// The 06 §8 metric: after local state is lost (restore from an old backup),
// discovery re-adopts OUR labeled servers as managed — never undeletable —
// and the class label restores the reclaim policy that eventually deletes
// the idle machine.
func TestRestore_ReadoptsLabeledServerAsManaged(t *testing.T) {
	h := newHarness(t, fake.Options{})
	enableDiscovery(h, reconcile.DiscoveryConfig{}, nil)

	acq := h.Acquire(app.AcquireCmd{Class: "ci-reclaim"})
	h.DriveAll()
	if err := h.Svc.Release(context.Background(), string(acq), "test"); err != nil {
		t.Fatal(err)
	}

	// "Restore from an old backup": the record vanishes; the cloud keeps
	// the machine. (Tombstoning the row models the lost-state lineage.)
	list, _ := h.St.Resources().List(context.Background(), storage.ResourceFilter{Kind: "compute.machine"})
	orig := list[0]
	if err := h.St.Tx(context.Background(), func(tx storage.TxStore) error {
		return tx.Resources().MarkDeleted(context.Background(), orig.ID, h.Clock.Now().UnixMilli())
	}); err != nil {
		t.Fatal(err)
	}

	sweep(t, h)

	live, _ := h.St.Resources().List(context.Background(), storage.ResourceFilter{Kind: "compute.machine"})
	if len(live) != 1 {
		t.Fatalf("re-adoption minted %d records, want 1", len(live))
	}
	re := live[0]
	if re.Ownership != storage.OwnershipManaged {
		t.Fatalf("re-adopted ownership = %s, want managed (06 §8)", re.Ownership)
	}
	if re.Class != "ci-reclaim" {
		t.Fatalf("class label not restored: %q", re.Class)
	}

	// The restored reclaim policy eventually deletes the idle machine.
	h.Clock.Advance(11 * time.Minute)
	if n, err := h.Rec.ReclaimPoolless(context.Background()); err != nil || n != 1 {
		t.Fatalf("reclaim after re-adoption: %d %v", n, err)
	}
	h.DriveAll()
	if n := h.CloudObjects(); n != 0 {
		t.Fatalf("re-adopted machine never deleted: %d objects (06 §8 broken)", n)
	}
}

// A ghost — our labels, but its create op succeeded bound to a DIFFERENT
// server — is collapsed via a normal journaled delete (plan R8).
func TestGhost_CollapsedViaJournaledDelete(t *testing.T) {
	h := newHarness(t, fake.Options{})
	enableDiscovery(h, reconcile.DiscoveryConfig{GhostPolicy: "delete"}, nil)

	resID := h.Create("real")
	h.Drive(resID)
	res, _ := h.St.Resources().Get(context.Background(), resID)
	op := res.Labels // not the op — find via events
	_ = op
	evs, _ := h.St.Events().List(context.Background(), storage.EventFilter{ResourceID: &resID}, 10)
	var opID string
	for _, ev := range evs {
		if ev.OperationID != nil {
			opID = string(*ev.OperationID)
			break
		}
	}

	// The duplicate-create race left a second server carrying the SAME op
	// label, bound to nothing.
	ghost := h.Fake.InjectRunning("ghost", map[string]string{
		provider.LabelManaged: "true",
		provider.LabelOwner:   h.OwnerID(),
		provider.LabelID:      string(resID),
		provider.LabelOp:      opID,
	})

	sweep(t, h)
	h.DriveAll()

	if _, err := h.Fake.Get(context.Background(), ghost); !provider.IsClass(err, provider.ErrNotFound) {
		t.Fatalf("ghost still alive: %v — it would bill forever", err)
	}
	// The real machine is untouched.
	if _, err := h.Fake.Get(context.Background(), provider.ExternalRef{ID: *res.ExternalID}); err != nil {
		t.Fatalf("REAL machine was deleted: %v", err)
	}
	if n := h.CloudObjects(); n != 1 {
		t.Fatalf("cloud has %d machines, want 1", n)
	}
}

// Orphan gates (ADR-017): confirmation → grace → tombstone; a leased record
// is NEVER tombstoned; an unhealthy provider suspends orphaning entirely.
func TestOrphan_GatedTombstone(t *testing.T) {
	h := newHarness(t, fake.Options{})
	enableDiscovery(h, reconcile.DiscoveryConfig{OrphanGrace: 30 * time.Second}, nil)

	resID := h.Create("doomed")
	h.Drive(resID)
	res, _ := h.St.Resources().Get(context.Background(), resID)

	// The machine vanishes out-of-band.
	if _, err := h.Fake.Apply(context.Background(), provider.Action{
		ActionID: "op_oob", Kind: "delete", Ref: &provider.ExternalRef{ID: *res.ExternalID}, Destructive: true,
	}); err != nil {
		t.Fatal(err)
	}

	sweep(t, h) // first confirmation → orphaned
	res, _ = h.St.Resources().Get(context.Background(), resID)
	if res.Phase != phase.Orphaned || res.DeletedAt != nil {
		t.Fatalf("after first sweep: phase=%s deleted=%v, want orphaned + live", res.Phase, res.DeletedAt)
	}

	sweep(t, h) // inside grace: still no tombstone
	res, _ = h.St.Resources().Get(context.Background(), resID)
	if res.DeletedAt != nil {
		t.Fatal("tombstoned inside the grace window")
	}

	h.Clock.Advance(31 * time.Second)
	sweep(t, h) // past grace, zero leases → tombstone
	res, _ = h.St.Resources().Get(context.Background(), resID)
	if res.DeletedAt == nil {
		t.Fatal("orphan never tombstoned after grace")
	}
}

func TestOrphan_LeasedRecordNeverTombstoned(t *testing.T) {
	h := newHarness(t, fake.Options{})
	enableDiscovery(h, reconcile.DiscoveryConfig{OrphanGrace: time.Second}, nil)

	acq := h.Acquire(app.AcquireCmd{Class: "ci"})
	h.DriveAll()
	got := h.acq(acq)
	lease, _ := h.St.Leases().Get(context.Background(), *got.LeaseID)
	res, _ := h.St.Resources().Get(context.Background(), lease.ResourceID)

	// Provider loses the leased machine (incident).
	if _, err := h.Fake.Apply(context.Background(), provider.Action{
		ActionID: "op_incident", Kind: "delete", Ref: &provider.ExternalRef{ID: *res.ExternalID}, Destructive: true,
	}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		sweep(t, h)
		h.Clock.Advance(time.Minute)
	}
	res, _ = h.St.Resources().Get(context.Background(), res.ID)
	if res.Phase != phase.Orphaned {
		t.Fatalf("phase = %s, want orphaned (leases stay active)", res.Phase)
	}
	if res.DeletedAt != nil {
		t.Fatal("LEASED record was tombstoned (invariant 3's spirit broken)")
	}
}

func TestOrphan_SuppressedWhenProviderUnhealthy(t *testing.T) {
	h := newHarness(t, fake.Options{})
	enableDiscovery(h, reconcile.DiscoveryConfig{}, func(string) bool { return false })

	resID := h.Create("survivor")
	h.Drive(resID)
	res, _ := h.St.Resources().Get(context.Background(), resID)
	if _, err := h.Fake.Apply(context.Background(), provider.Action{
		ActionID: "op_oob2", Kind: "delete", Ref: &provider.ExternalRef{ID: *res.ExternalID}, Destructive: true,
	}); err != nil {
		t.Fatal(err)
	}

	sweep(t, h)
	res, _ = h.St.Resources().Get(context.Background(), resID)
	if res.Phase == phase.Orphaned {
		t.Fatal("resource orphaned while the provider was unhealthy — a cloud outage must not reclaim VMs")
	}
}

// Foreign unlabeled resources become read-only observed records (07 §5);
// deleting them is refused (invariant 5, both directions with managed).
func TestAdoptUnlabeled_ObservedIsReadOnly(t *testing.T) {
	h := newHarness(t, fake.Options{})
	enableDiscovery(h, reconcile.DiscoveryConfig{AdoptUnlabeled: "observed"}, nil)

	h.Fake.InjectRunning("someone-elses-vm", map[string]string{"team": "other"})
	sweep(t, h)

	list, _ := h.St.Resources().List(context.Background(), storage.ResourceFilter{
		Ownership: []storage.Ownership{storage.OwnershipObserved},
	})
	if len(list) != 1 {
		t.Fatalf("observed records = %d, want 1", len(list))
	}

	_, err := h.Svc.DeleteResource(context.Background(), app.DeleteResourceCmd{
		ID: string(list[0].ID), Actor: "test",
		BuildResponse: func(*storage.Resource) (int, json.RawMessage) { return 202, json.RawMessage(`{}`) },
	})
	var ve *app.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("deleting an observed resource: %v, want refusal (invariant 5)", err)
	}
	if n := h.CloudObjects(); n != 1 {
		t.Fatal("foreign machine touched")
	}
}

// Deletion safeguards: dryRun reports the plan without mutating; the
// protected flag blocks deletion.
func TestDelete_DryRunAndProtected(t *testing.T) {
	h := newHarness(t, fake.Options{})
	resID := h.Create("careful")
	h.Drive(resID)

	out, err := h.Svc.DeleteResource(context.Background(), app.DeleteResourceCmd{
		ID: string(resID), Actor: "test", DryRun: true,
		BuildResponse: func(r *storage.Resource) (int, json.RawMessage) {
			return 200, json.RawMessage(`{"wouldDelete":true}`)
		},
	})
	if err != nil || out.Status != 200 {
		t.Fatalf("dry run: %+v %v", out, err)
	}
	res, _ := h.St.Resources().Get(context.Background(), resID)
	if res.Phase != phase.Ready || res.DeletedAt != nil {
		t.Fatalf("dry run mutated state: %+v", res)
	}
	if n, _ := h.openOps(resID); n != 0 {
		t.Fatal("dry run journaled an operation")
	}

	// Protected blocks the real delete.
	if err := h.St.Tx(context.Background(), func(tx storage.TxStore) error {
		res.DeleteProtected = true
		res.UpdatedAt = h.Clock.Now().UnixMilli()
		return tx.Resources().UpdateSpec(context.Background(), res, res.Generation)
	}); err != nil {
		t.Fatal(err)
	}
	_, err = h.Svc.DeleteResource(context.Background(), app.DeleteResourceCmd{
		ID: string(resID), Actor: "test",
		BuildResponse: func(*storage.Resource) (int, json.RawMessage) { return 202, json.RawMessage(`{}`) },
	})
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("protected delete: %v, want conflict", err)
	}
}
