// Package conformance is the reusable provider test kit (docs/03 §8):
// provider authors run the same suite Fleetplane runs against its fake and
// (env-gated) against real clouds. Subtest names are the contract.
package conformance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
)

// Harness supplies the provider under test and kind-specific inputs.
type Harness struct {
	Provider provider.Provider
	Kind     provider.ResourceKind

	// NewSpec generates a valid, distinct kind spec for index i.
	NewSpec func(i int) json.RawMessage
	// InvalidSpec must be rejected with ErrInvalid.
	InvalidSpec json.RawMessage

	// OwnerID is the control-plane identity used for ownership labels.
	OwnerID string
	// Eventual: the provider may lag list-after-create; polls are bounded
	// by MaxSteps either way.
	Eventual bool
	// Expensive enables bulk subtests (pagination); keep off against real
	// clouds unless explicitly wanted.
	Expensive bool
	// MaxSteps bounds every polling loop (default 200).
	MaxSteps int
}

// Run executes the conformance catalog.
func Run(t *testing.T, h Harness) {
	if h.MaxSteps <= 0 {
		h.MaxSteps = 200
	}
	driver, ok := h.Provider.ResourceDriver(h.Kind)
	if !ok {
		t.Fatalf("provider does not drive kind %s", h.Kind)
	}
	c := &checker{h: h, driver: driver}

	t.Run("Descriptor/DeclaresKind", c.descriptorDeclaresKind)
	t.Run("Errors/GetMissingIsErrNotFound", c.getMissingIsNotFound)
	t.Run("Errors/InvalidSpecIsErrInvalid", c.invalidSpecIsInvalid)
	t.Run("Lifecycle/CreateObserveGetDelete", c.lifecycle)
	t.Run("Lifecycle/DeleteOfDeleted", c.deleteOfDeleted)
	t.Run("Idempotency/OpLabelDedup", c.opLabelDedup)
	t.Run("EventualConsistency/GetBeforeList", c.getBeforeList)
	t.Run("Labels/OwnershipApplied", c.ownershipApplied)
	t.Run("Discovery/Stability", c.discoveryStability)
	t.Run("Discovery/OwnedScopeOnlyOwned", c.ownedScopeOnlyOwned)
	t.Run("Operations/PollingReachesTerminal", c.lifecycle) // same proof, named per 03 §8
	t.Run("Billing/CapabilityContract", c.billingContract)
	if h.Expensive {
		t.Run("Pagination/OverOnePage", c.pagination)
	}
}

type checker struct {
	h      Harness
	driver provider.ResourceDriver
	n      int
}

func (c *checker) uid() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// create plans+applies one resource and polls its operation to success,
// registering cleanup. Returns the external ref.
func (c *checker) create(t *testing.T) provider.ExternalRef {
	t.Helper()
	c.n++
	resID := "res-conf-" + c.uid()
	opID := "op-conf-" + c.uid()

	plan, err := c.driver.Plan(context.Background(), provider.PlanRequest{
		ResourceID: resID,
		Desired: &provider.DesiredState{
			Name:   "conf-" + c.uid(),
			Spec:   c.h.NewSpec(c.n),
			Labels: provider.IdentityLabels(c.h.OwnerID, resID, opID),
		},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Kind == "" {
		t.Fatalf("create plan = %+v, want one action", plan)
	}
	action := plan.Actions[0]
	action.ActionID = opID
	opRef, err := c.driver.Apply(context.Background(), action)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if opRef.Ref == nil || opRef.Ref.ID == "" {
		t.Fatal("Apply(create) returned no accept-time ref (05 §10)")
	}
	ref := *opRef.Ref
	t.Cleanup(func() { c.destroy(t, ref) })
	c.pollToSuccess(t, opRef)
	return ref
}

func (c *checker) pollToSuccess(t *testing.T, opRef provider.OperationRef) {
	t.Helper()
	for i := 0; i < c.h.MaxSteps; i++ {
		status, err := c.driver.ObserveOperation(context.Background(), opRef)
		if err != nil {
			if provider.IsClass(err, provider.ErrRateLimited) || provider.IsClass(err, provider.ErrRetryable) {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			t.Fatalf("ObserveOperation: %v", err)
		}
		switch status.State {
		case provider.OpSucceeded:
			return
		case provider.OpFailed:
			t.Fatalf("operation failed: %v", status.Failure)
		}
	}
	t.Fatalf("operation did not reach a terminal state in %d observations", c.h.MaxSteps)
}

func (c *checker) destroy(t *testing.T, ref provider.ExternalRef) {
	t.Helper()
	opID := "op-conf-del-" + c.uid()
	opRef, err := c.driver.Apply(context.Background(), provider.Action{
		ActionID: opID, Kind: "delete", ResourceID: "cleanup", Ref: &ref, Destructive: true,
	})
	if err != nil {
		if provider.IsClass(err, provider.ErrNotFound) {
			return
		}
		t.Errorf("cleanup delete: %v", err)
		return
	}
	for i := 0; i < c.h.MaxSteps; i++ {
		status, err := c.driver.ObserveOperation(context.Background(), opRef)
		if err != nil || status.State == provider.OpSucceeded || status.State == provider.OpFailed {
			return
		}
	}
}

// --- subtests ---

func (c *checker) descriptorDeclaresKind(t *testing.T) {
	desc := c.h.Provider.Descriptor()
	for _, k := range desc.Kinds {
		if k == c.h.Kind {
			return
		}
	}
	t.Fatalf("descriptor kinds %v missing %s", desc.Kinds, c.h.Kind)
}

func (c *checker) getMissingIsNotFound(t *testing.T) {
	_, err := c.driver.Get(context.Background(), provider.ExternalRef{ID: "does-not-exist-" + c.uid()})
	if !provider.IsClass(err, provider.ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want *Error{Class: ErrNotFound} — never (zero, nil)", err)
	}
}

func (c *checker) invalidSpecIsInvalid(t *testing.T) {
	if len(c.h.InvalidSpec) == 0 {
		t.Skip("harness provides no InvalidSpec")
	}
	_, err := c.driver.Plan(context.Background(), provider.PlanRequest{
		ResourceID: "res-conf-invalid",
		Desired:    &provider.DesiredState{Name: "x", Spec: c.h.InvalidSpec},
	})
	if err == nil {
		t.Fatal("invalid spec accepted by Plan")
	}
	if !provider.IsClass(err, provider.ErrInvalid) {
		t.Fatalf("invalid spec classified %q, want invalid (fail fast, not retry)", provider.Classify(err))
	}
	if provider.Effect(err) != provider.EffectNone {
		t.Fatal("validation failure must be EffectNone")
	}
}

func (c *checker) lifecycle(t *testing.T) {
	ref := c.create(t)
	obs, err := c.driver.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	if obs.Phase != provider.PhaseRunning {
		t.Fatalf("created resource phase = %s, want running", obs.Phase)
	}
	if len(obs.Extensions) == 0 {
		t.Fatal("Extensions empty (invariant 6)")
	}
	c.destroy(t, ref)
	if _, err := c.driver.Get(context.Background(), ref); !provider.IsClass(err, provider.ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
}

func (c *checker) deleteOfDeleted(t *testing.T) {
	ref := c.create(t)
	c.destroy(t, ref)
	// Second delete must be success or not_found — never an error loop.
	opRef, err := c.driver.Apply(context.Background(), provider.Action{
		ActionID: "op-conf-dd-" + c.uid(), Kind: "delete", ResourceID: "x", Ref: &ref, Destructive: true,
	})
	if err != nil {
		if provider.IsClass(err, provider.ErrNotFound) {
			return
		}
		t.Fatalf("delete of deleted: %v", err)
	}
	// For delete operations, not_found while polling IS success.
	for i := 0; i < c.h.MaxSteps; i++ {
		status, err := c.driver.ObserveOperation(context.Background(), opRef)
		if provider.IsClass(err, provider.ErrNotFound) {
			return
		}
		if err != nil {
			t.Fatalf("ObserveOperation: %v", err)
		}
		if status.State == provider.OpSucceeded {
			return
		}
		if status.State == provider.OpFailed {
			t.Fatalf("delete of deleted failed: %v", status.Failure)
		}
	}
	t.Fatalf("delete of deleted never terminal in %d observations", c.h.MaxSteps)
}

func (c *checker) opLabelDedup(t *testing.T) {
	c.n++
	resID, opID := "res-conf-"+c.uid(), "op-conf-"+c.uid()
	plan, err := c.driver.Plan(context.Background(), provider.PlanRequest{
		ResourceID: resID,
		Desired: &provider.DesiredState{
			Name: "dedup-" + c.uid(), Spec: c.h.NewSpec(c.n),
			Labels: provider.IdentityLabels(c.h.OwnerID, resID, opID),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	action := plan.Actions[0]
	action.ActionID = opID
	ref1, err := c.driver.Apply(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.destroy(t, *ref1.Ref) })
	ref2, err := c.driver.Apply(context.Background(), action) // journal replay
	if err != nil {
		t.Fatalf("replayed Apply errored: %v", err)
	}
	if ref1.Ref.ID != ref2.Ref.ID {
		t.Fatalf("same ActionID produced two resources: %s vs %s (invariant 7 anchor broken)", ref1.Ref.ID, ref2.Ref.ID)
	}
}

func (c *checker) getBeforeList(t *testing.T) {
	ref := c.create(t)
	// Get by ref must be authoritative even while lists lag (05 §10).
	if _, err := c.driver.Get(context.Background(), ref); err != nil {
		t.Fatalf("Get by ref after accepted create: %v", err)
	}
}

func (c *checker) ownershipApplied(t *testing.T) {
	ref := c.create(t)
	obs, err := c.driver.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{provider.LabelManaged, provider.LabelOwner, provider.LabelID, provider.LabelOp} {
		if obs.Labels[key] == "" {
			t.Errorf("created resource missing label %s", key)
		}
	}
	if !obs.Owned {
		t.Fatal("created resource not recognized as Owned")
	}
	if obs.FleetplaneID == "" || obs.CreateOpID == "" {
		t.Fatal("FleetplaneID/CreateOpID not parsed from labels")
	}
}

func (c *checker) discoveryStability(t *testing.T) {
	ref := c.create(t)
	seen := func() map[string]bool {
		out := map[string]bool{}
		for i := 0; i < c.h.MaxSteps; i++ {
			list, err := c.driver.Discover(context.Background(), provider.DiscoverRequest{Scope: provider.ScopeOwned})
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range list {
				out[o.Ref.ID] = true
			}
			if out[ref.ID] {
				return out
			}
		}
		t.Fatalf("created resource never became discoverable within %d lists", c.h.MaxSteps)
		return nil
	}
	first := seen()
	second := seen()
	for id := range first {
		if !second[id] {
			t.Fatalf("discovery unstable: %s vanished between consecutive lists", id)
		}
	}
}

func (c *checker) ownedScopeOnlyOwned(t *testing.T) {
	c.create(t)
	for i := 0; i < c.h.MaxSteps; i++ {
		list, err := c.driver.Discover(context.Background(), provider.DiscoverRequest{Scope: provider.ScopeOwned})
		if err != nil {
			t.Fatal(err)
		}
		if len(list) > 0 {
			for _, o := range list {
				if !o.Owned {
					t.Fatalf("ScopeOwned returned a non-owned resource: %+v", o)
				}
			}
			return
		}
	}
	t.Fatal("ScopeOwned never returned our resources")
}

func (c *checker) pagination(t *testing.T) {
	const total = 5
	refs := map[string]bool{}
	for i := 0; i < total; i++ {
		refs[c.create(t).ID] = true
	}
	deadline := 0
	for {
		list, err := c.driver.Discover(context.Background(), provider.DiscoverRequest{Scope: provider.ScopeOwned})
		if err != nil {
			t.Fatal(err)
		}
		got := 0
		for _, o := range list {
			if refs[o.Ref.ID] {
				got++
			}
		}
		if got == total {
			return
		}
		deadline++
		if deadline > c.h.MaxSteps {
			t.Fatalf("pagination lost resources: saw %d of %d", got, total)
		}
	}
}

// billingContract validates the optional BillingAware capability (docs/11
// §3): non-negative durations, zero policy for undeclared kinds, and
// stability across calls. Skipped for providers without the capability.
func (c *checker) billingContract(t *testing.T) {
	ba, ok := c.h.Provider.(provider.BillingAware)
	if !ok {
		t.Skip("provider does not implement BillingAware")
	}
	pol := ba.Billing(c.h.Kind)
	if pol.MinimumDuration < 0 || pol.BillingIncrement < 0 || pol.TerminationBuffer < 0 {
		t.Fatalf("billing durations must be >= 0: %+v", pol)
	}
	if again := ba.Billing(c.h.Kind); again != pol {
		t.Fatalf("billing policy unstable across calls: %+v vs %+v", pol, again)
	}
	if und := ba.Billing(provider.ResourceKind("conformance.undeclared/kind")); !und.FineGrained() {
		t.Fatalf("undeclared kind must return the zero policy, got %+v", und)
	}
}
