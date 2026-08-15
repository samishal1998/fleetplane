package fake

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/samishal1998/fleetplane/pkg/kinds/compute"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
)

func mustCreate(t *testing.T, f *Fake, resID, opID, ownerID string) provider.ExternalRef {
	t.Helper()
	ctx := context.Background()
	spec, _ := json.Marshal(compute.MachineSpec{ServerType: "cpx31", Image: "snapshot:ci=1"})
	plan, err := f.Plan(ctx, provider.PlanRequest{
		ResourceID: resID,
		Desired: &provider.DesiredState{
			Name:   "m-" + resID,
			Spec:   spec,
			Labels: provider.IdentityLabels(ownerID, resID, opID),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != "create" {
		t.Fatalf("plan = %+v, want one create", plan)
	}
	a := plan.Actions[0]
	a.ActionID = opID // kernel stamps ActionID := OperationID after Plan (R2)
	ref, err := f.Apply(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Ref == nil || ref.Ref.ID == "" {
		t.Fatal("create returned no external ref")
	}
	return *ref.Ref
}

func TestCreateDiscoverOwnedOnly(t *testing.T) {
	f := New("fake-1", "owner-A", Options{})
	ref := mustCreate(t, f, "res_1", "op_1", "owner-A")
	mustCreate(t, f, "res_2", "op_2", "owner-B") // foreign control plane

	got, err := f.Discover(context.Background(), provider.DiscoverRequest{Scope: provider.ScopeOwned})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Ref.ID != ref.ID {
		t.Fatalf("ScopeOwned returned %d resources, want exactly our own", len(got))
	}
	if !got[0].Owned || got[0].FleetplaneID != "res_1" || got[0].CreateOpID != "op_1" {
		t.Fatalf("observation labels not parsed: %+v", got[0])
	}

	all, err := f.Discover(context.Background(), provider.DiscoverRequest{Scope: provider.ScopeAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("ScopeAll returned %d, want 2", len(all))
	}
}

func TestDiscoverByOpLabel(t *testing.T) {
	f := New("fake-1", "owner-A", Options{})
	ref := mustCreate(t, f, "res_1", "op_dedup", "owner-A")
	got, err := f.Discover(context.Background(), provider.DiscoverRequest{
		Scope:    provider.ScopeOwned,
		Selector: map[string]string{provider.LabelOp: "op_dedup"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Ref.ID != ref.ID {
		t.Fatal("discovery by op label failed — crash resolution depends on this (invariant 7)")
	}
}

func TestApplySameActionIDTwiceOneResource(t *testing.T) {
	f := New("fake-1", "owner-A", Options{})
	mustCreate(t, f, "res_1", "op_same", "owner-A")
	// Re-apply the same create (journal replay after crash).
	spec, _ := json.Marshal(compute.MachineSpec{ServerType: "cpx31", Image: "snapshot:ci=1"})
	params, _ := json.Marshal(createParams{Name: "m-res_1", Spec: compute.MachineSpec{ServerType: "cpx31", Image: "snapshot:ci=1"},
		Labels: provider.IdentityLabels("owner-A", "res_1", "op_same")})
	_ = spec
	ref2, err := f.Apply(context.Background(), provider.Action{ActionID: "op_same", Kind: "create", ResourceID: "res_1", Params: params})
	if err != nil {
		t.Fatal(err)
	}
	all, _ := f.Discover(context.Background(), provider.DiscoverRequest{Scope: provider.ScopeAll})
	if len(all) != 1 {
		t.Fatalf("same ActionID produced %d objects, want 1 (invariant 2 defense in depth)", len(all))
	}
	if ref2.Ref.ID != all[0].Ref.ID {
		t.Fatal("replayed create returned a different ref")
	}
}

func TestGetMissingIsErrNotFound(t *testing.T) {
	f := New("fake-1", "owner-A", Options{})
	_, err := f.Get(context.Background(), provider.ExternalRef{ID: "999999"})
	if !provider.IsClass(err, provider.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDeleteOfDeletedIsSuccess(t *testing.T) {
	f := New("fake-1", "owner-A", Options{})
	ref := mustCreate(t, f, "res_1", "op_1", "owner-A")
	del := provider.Action{ActionID: "op_d1", Kind: "delete", ResourceID: "res_1", Ref: &ref, Destructive: true}
	if _, err := f.Apply(context.Background(), del); err != nil {
		t.Fatal(err)
	}
	// Second delete: not an error (docs/03 §6, FI-7).
	del.ActionID = "op_d2"
	if _, err := f.Apply(context.Background(), del); err != nil {
		t.Fatalf("delete of deleted errored: %v", err)
	}
	if _, err := f.Get(context.Background(), ref); !provider.IsClass(err, provider.ErrNotFound) {
		t.Fatal("deleted machine still visible")
	}
}

func TestExtensionsPresent(t *testing.T) {
	f := New("fake-1", "owner-A", Options{})
	ref := mustCreate(t, f, "res_1", "op_1", "owner-A")
	obs, err := f.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.Extensions) == 0 {
		t.Fatal("Extensions empty; provider-native data must flow through (invariant 6)")
	}
	if obs.Capacity[compute.DimCPU] <= 0 {
		t.Fatal("capacity missing")
	}
}

func TestRegisteredInGlobalRegistry(t *testing.T) {
	for _, d := range provider.Drivers() {
		if d == Driver {
			return
		}
	}
	t.Fatalf("driver %q not registered via init()", Driver)
}
