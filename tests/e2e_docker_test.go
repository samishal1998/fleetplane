package tests

// Full-kernel end-to-end against a REAL provider that needs no credentials:
// Docker containers stand in for VMs (ADR-020). The whole stack — service,
// scheduler, operation journal, reclaim, park/resume, crash recovery — runs
// unchanged; only the driver differs. Skips when no daemon is reachable
// unless FLEETPLANE_E2E_DOCKER=1 (CI) turns the skip into a failure.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samishal1998/fleetplane/internal/app"
	"github.com/samishal1998/fleetplane/internal/operations"
	"github.com/samishal1998/fleetplane/internal/phase"
	"github.com/samishal1998/fleetplane/internal/reconcile"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/pkg/kinds/compute"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
	"github.com/samishal1998/fleetplane/providers/docker"
	"github.com/samishal1998/fleetplane/providers/fake"
)

const dockerSpec = `{"serverType":"1x64","image":"name:alpine:3.20","labels":{"fleetplane.io/test":"1"}}`

func newDockerHarness(t *testing.T) *Harness {
	t.Helper()
	probe, err := docker.New(provider.InstanceConfig{Instance: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.Health(context.Background()); err != nil {
		if os.Getenv("FLEETPLANE_E2E_DOCKER") == "1" {
			t.Fatalf("FLEETPLANE_E2E_DOCKER=1 but docker unreachable: %v", err)
		}
		t.Skipf("docker unreachable, skipping: %v", err)
	}
	h := &Harness{
		t:              t,
		dbPath:         filepath.Join(t.TempDir(), "fp.db"),
		Clock:          newStepClock(),
		extraProviders: []app.ProviderSpec{{Name: "docker-local", Driver: "docker"}},
		extraClasses: map[string]reconcile.Class{
			"docker": {
				Kind: "compute.machine", Provider: "docker-local",
				Spec: json.RawMessage(dockerSpec),
				Reclaim: &reconcile.ReclaimPolicy{
					IdleAfter:   compute.Duration(10 * time.Minute),
					DeleteAfter: compute.Duration(time.Hour),
				},
			},
		},
	}
	h.open(nil, fake.Options{})
	t.Cleanup(func() {
		// Sweep by owner + test label so a failed run never leaks containers.
		list, _ := h.dockerDriver().Discover(context.Background(), provider.DiscoverRequest{
			Scope: provider.ScopeOwned, Selector: map[string]string{provider.LabelTest: "1"},
		})
		for _, c := range list {
			_, _ = h.dockerDriver().Apply(context.Background(), provider.Action{ActionID: "sweep", Kind: "delete", Ref: &c.Ref, Destructive: true})
		}
	})
	return h
}

func (h *Harness) dockerDriver() provider.ResourceDriver {
	inst, _ := h.Providers.Instance("docker-local")
	drv, _ := inst.ResourceDriver(compute.Kind)
	return drv
}

// containers returns the owned containers as seen by the daemon — the
// ground truth for "how many machines really exist".
func (h *Harness) containers() []provider.ObservedResource {
	h.t.Helper()
	list, err := h.dockerDriver().Discover(context.Background(), provider.DiscoverRequest{Scope: provider.ScopeOwned})
	if err != nil {
		h.t.Fatal(err)
	}
	return list
}

func (h *Harness) oneContainer(wantState string) provider.ObservedResource {
	h.t.Helper()
	list := h.containers()
	if len(list) != 1 || list[0].ProviderState != wantState {
		h.t.Fatalf("want exactly one %s container, got %d: %+v", wantState, len(list), states(list))
	}
	return list[0]
}

func states(list []provider.ObservedResource) []string {
	out := make([]string, len(list))
	for i, c := range list {
		out[i] = c.ProviderState
	}
	return out
}

// The demo flow (08 §7) over real containers: acquire creates and binds;
// release + idle parks (container stopped, not removed); the next acquire
// resumes the same container with zero creates; stage 2 removes it.
func TestE2E_DockerLeaseParkResumeDelete(t *testing.T) {
	h := newDockerHarness(t)
	ctx := context.Background()

	a := h.Acquire(app.AcquireCmd{Class: "docker", TTL: time.Minute})
	h.DriveAll()
	if got := h.acq(a).State; got != storage.AcqBound {
		t.Fatalf("acquisition state = %s, want bound", got)
	}
	first := h.oneContainer("running")
	if first.Capacity[compute.DimCPU] != 1 || first.Capacity[compute.DimMemoryMiB] != 64 {
		t.Fatalf("container limits not applied: %v", first.Capacity)
	}
	resID := storage.ResourceID(first.FleetplaneID)

	if err := h.Svc.Release(ctx, string(a), "test"); err != nil {
		t.Fatal(err)
	}
	h.Clock.Advance(11 * time.Minute)
	if n, _ := h.Rec.ReclaimPoolless(ctx); n != 1 {
		t.Fatal("stage-1 park not journaled")
	}
	h.DriveAll()
	if r, _ := h.St.Resources().Get(ctx, resID); r.Phase != phase.Parked {
		t.Fatalf("phase after idle = %s, want parked", r.Phase)
	}
	h.oneContainer("exited")

	b := h.Acquire(app.AcquireCmd{Class: "docker", TTL: time.Minute})
	h.DriveAll()
	acq := h.acq(b)
	if acq.State != storage.AcqBound {
		t.Fatalf("second acquisition state = %s", acq.State)
	}
	if l, _ := h.St.Leases().Get(ctx, *acq.LeaseID); l.ResourceID != resID {
		t.Fatalf("bound to %s, want the parked %s (start-before-create)", l.ResourceID, resID)
	}
	if c := h.oneContainer("running"); c.Ref.ID != first.Ref.ID {
		t.Fatal("resume must reuse the same container, not create another")
	}

	if err := h.Svc.Release(ctx, string(b), "test"); err != nil {
		t.Fatal(err)
	}
	h.Clock.Advance(11 * time.Minute)
	_, _ = h.Rec.ReclaimPoolless(ctx) // park again
	h.DriveAll()
	h.Clock.Advance(2 * time.Hour)
	if n, _ := h.Rec.ReclaimPoolless(ctx); n != 1 {
		t.Fatal("stage-2 delete not journaled")
	}
	h.DriveAll()
	if list := h.containers(); len(list) != 0 {
		t.Fatalf("container survived stage-2 delete: %v", states(list))
	}
	if r, err := h.St.Resources().Get(ctx, resID); err == nil && r.DeletedAt == nil {
		t.Fatalf("resource not tombstoned after delete: phase=%s", r.Phase)
	}
}

// Invariant 7 against a real provider: crash after the create is journaled
// but before it is dispatched, then crash again mid-flight after the
// container exists; recovery converges to exactly one container.
func TestE2E_DockerCrashRecoveryNeverDuplicates(t *testing.T) {
	h := newDockerHarness(t)
	ctx := context.Background()

	create := func(name string) storage.ResourceID {
		out, err := h.Svc.CreateResource(ctx, app.CreateResourceCmd{
			Kind: "compute.machine", Provider: "docker-local", Name: name,
			Spec: json.RawMessage(dockerSpec), Actor: "test",
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
		return storage.ResourceID(body.ID)
	}

	// Crash 1: journaled, never dispatched (TxA only).
	res := create("crash-journaled")
	h.Crash()
	h.Restart()
	h.Drive(res)
	c := h.oneContainer("running")
	if c.FleetplaneID != string(res) {
		t.Fatalf("container labeled %s, want %s", c.FleetplaneID, res)
	}

	// Crash 2 (FI-3 shape): the container exists but the op is still
	// in_flight — the verifier must find it by op label and adopt, not
	// re-create.
	armed := true
	h.Hooks = operations.Hooks{AfterApply: func(storage.OperationID) {
		if armed {
			panic("simulated crash between Apply and TxC")
		}
	}}
	h.open(h.Providers, fake.Options{}) // rebuild engine with hooks
	res2 := create("crash-in-flight")
	func() {
		defer func() { _ = recover() }()
		_, _ = h.Engine.Step(ctx)
	}()
	if op, err := h.opFor(res2); err != nil || op.State != storage.OpInFlight {
		t.Fatalf("precondition: op = %v (%v), want in_flight", op, err)
	}
	if n := len(h.containers()); n != 2 {
		t.Fatalf("precondition: container should exist after Apply, got %d", n)
	}
	armed = false
	h.Crash()
	h.Hooks = operations.Hooks{}
	h.Restart()
	h.Drive(res2)

	list := h.containers()
	if len(list) != 2 {
		t.Fatalf("want exactly two containers after two crash recoveries, got %d: %v", len(list), states(list))
	}
	for _, c := range list {
		if c.ProviderState != "running" {
			t.Fatalf("container %s is %s", c.FleetplaneID, c.ProviderState)
		}
	}
	if r, _ := h.St.Resources().Get(ctx, res2); r.Phase != phase.Ready || r.ExternalID == nil {
		t.Fatalf("adopted resource: phase=%s external=%v", r.Phase, r.ExternalID)
	}
}
