package gcp

// Stop/start (docs/12 parked machines) driver tests: idempotency both ways,
// per-op-kind terminal predicates, the fresh-address contract after start,
// vanished => ErrNotFound, and the local-SSD 400 => ErrInvalid clean revert.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/samishal1998/fleetplane/pkg/kinds/compute"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
)

func ci1Ref() *provider.ExternalRef {
	return &provider.ExternalRef{ID: "ci-1", Extra: map[string]string{"zone": testZone}}
}

func parkAction(kind string) provider.Action {
	return provider.Action{ActionID: "op_01PARK", Kind: kind, Ref: ci1Ref()}
}

// --- capability ---

func TestGCPParkingPolicy(t *testing.T) {
	_, g := newGCEMock(t)
	var pa provider.ParkAware = g // compile-time: the driver is ParkAware
	pol := pa.Parking(compute.Kind)
	if !pol.Supported || pol.StartEstimate != 45*time.Second {
		t.Fatalf("Parking(compute.machine) = %+v, want {Supported:true StartEstimate:45s}", pol)
	}
	if again := pa.Parking(compute.Kind); again != pol {
		t.Fatalf("park policy unstable: %+v vs %+v", pol, again)
	}
	for _, kind := range []provider.ResourceKind{"storage.volume", "network.loadbalancer", "unknown.kind"} {
		if got := pa.Parking(kind); got != (provider.ParkPolicy{}) {
			t.Fatalf("Parking(%s) = %+v, want the zero policy", kind, got)
		}
	}
}

// --- happy paths ---

func TestGCPStop_HappyPath(t *testing.T) {
	m, g := newGCEMock(t)
	opRef, err := g.Apply(context.Background(), parkAction("stop"))
	if err != nil {
		t.Fatal(err)
	}
	if m.stopCalls.Load() != 1 {
		t.Fatalf("stop calls = %d, want 1", m.stopCalls.Load())
	}
	var data opData
	if err := json.Unmarshal(opRef.Data, &data); err != nil ||
		data.Kind != "stop" || data.Op != "op-stop-1" || data.Zone != testZone || data.Instance != "ci-1" {
		t.Fatalf("stop op data = %s (err %v)", opRef.Data, err)
	}

	// Zonal op DONE + instance observed TERMINATED => success, PhaseStopped.
	m.instStatus.Store("TERMINATED")
	status, err := g.ObserveOperation(context.Background(), opRef)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != provider.OpSucceeded || status.Resource == nil {
		t.Fatalf("status = %+v", status)
	}
	if status.Resource.Phase != provider.PhaseStopped || status.Resource.ProviderState != "TERMINATED" {
		t.Fatalf("stop must report the stopped resource: %+v", status.Resource)
	}
	// Identity survives the stop (docs/12 §3).
	if status.Resource.FleetplaneID != testResID || status.Resource.CreateOpID != takenOpID || !status.Resource.Owned {
		t.Fatalf("identity lost across stop: %+v", status.Resource)
	}
}

func TestGCPStart_HappyPath_FreshAddresses(t *testing.T) {
	m, g := newGCEMock(t)
	m.instStatus.Store("TERMINATED")
	opRef, err := g.Apply(context.Background(), parkAction("start"))
	if err != nil {
		t.Fatal(err)
	}
	if m.startCalls.Load() != 1 {
		t.Fatalf("start calls = %d, want 1", m.startCalls.Load())
	}
	var data opData
	if err := json.Unmarshal(opRef.Data, &data); err != nil || data.Kind != "start" || data.Op != "op-start-1" {
		t.Fatalf("start op data = %s (err %v)", opRef.Data, err)
	}

	// Still STAGING (the mock's post-start state): not success yet.
	status, err := g.ObserveOperation(context.Background(), opRef)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != provider.OpRunning {
		t.Fatalf("start observed while STAGING = %+v, want running", status)
	}

	m.instStatus.Store("RUNNING")
	status, err = g.ObserveOperation(context.Background(), opRef)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != provider.OpSucceeded || status.Resource == nil || status.Resource.Phase != provider.PhaseRunning {
		t.Fatalf("status = %+v", status)
	}
	// GCE released the ephemeral IP at stop: the success snapshot must carry
	// the NEW address from the fresh instances.get, never the pre-stop one.
	var got []string
	for _, a := range status.Resource.Addresses {
		got = append(got, a.Addr)
	}
	found := false
	for _, a := range got {
		if a == "203.0.113.99" {
			found = true
		}
		if a == "203.0.113.5" {
			t.Fatalf("stale pre-stop address reported after start: %v", got)
		}
	}
	if !found {
		t.Fatalf("fresh post-start address missing: %v", got)
	}
}

// --- idempotency (docs/12 §3: the crash-recovery anchor) ---

func TestGCPStop_IdempotentOnStoppedFamily(t *testing.T) {
	for _, st := range []string{"TERMINATED", "STOPPED", "STOPPING", "SUSPENDED"} {
		t.Run(st, func(t *testing.T) {
			m, g := newGCEMock(t)
			m.instStatus.Store(st)
			opRef, err := g.Apply(context.Background(), parkAction("stop"))
			if err != nil {
				t.Fatalf("stop of %s must succeed (docs/12 §3): %v", st, err)
			}
			if m.stopCalls.Load() != 0 {
				t.Fatalf("stop of %s must not mutate: %d stop calls", st, m.stopCalls.Load())
			}
			var data opData
			if err := json.Unmarshal(opRef.Data, &data); err != nil || data.Kind != "stop" || data.Op != "" {
				t.Fatalf("idempotent stop Data must carry the kind and NO zonal op: %s (err %v)", opRef.Data, err)
			}

			// No zonal op => verify directly by instance state: OpRunning
			// while transitional (STOPPING), success in any settled
			// power-off state (TERMINATED/STOPPED/SUSPENDED) — every
			// transitional state stopSettled accepts converges to one, so a
			// no-op stop record can never poll forever.
			status, err := g.ObserveOperation(context.Background(), opRef)
			if err != nil {
				t.Fatal(err)
			}
			if st == "STOPPING" {
				if status.State != provider.OpRunning {
					t.Fatalf("observed %s: status = %+v, want running (predicate not yet held)", st, status)
				}
			} else if status.State != provider.OpSucceeded || status.Resource == nil || status.Resource.Phase != provider.PhaseStopped {
				t.Fatalf("observed %s: status = %+v, want succeeded/stopped", st, status)
			}
		})
	}
}

func TestGCPStart_IdempotentOnRunningFamily(t *testing.T) {
	for _, st := range []string{"RUNNING", "STAGING", "PROVISIONING"} {
		t.Run(st, func(t *testing.T) {
			m, g := newGCEMock(t)
			m.instStatus.Store(st)
			opRef, err := g.Apply(context.Background(), parkAction("start"))
			if err != nil {
				t.Fatalf("start of %s must succeed (docs/12 §3): %v", st, err)
			}
			if m.startCalls.Load() != 0 {
				t.Fatalf("start of %s must not mutate: %d start calls", st, m.startCalls.Load())
			}
			var data opData
			if err := json.Unmarshal(opRef.Data, &data); err != nil || data.Kind != "start" || data.Op != "" {
				t.Fatalf("idempotent start Data must carry the kind and NO zonal op: %s (err %v)", opRef.Data, err)
			}

			status, err := g.ObserveOperation(context.Background(), opRef)
			if err != nil {
				t.Fatal(err)
			}
			if st == "RUNNING" {
				if status.State != provider.OpSucceeded || status.Resource == nil || status.Resource.Phase != provider.PhaseRunning {
					t.Fatalf("status = %+v, want succeeded/running", status)
				}
			} else if status.State != provider.OpRunning {
				t.Fatalf("observed %s: status = %+v, want running (predicate not yet held)", st, status)
			}
		})
	}
}

// --- per-kind terminal predicates (the design blocker) ---

func TestGCPObserveStop_NotSuccessWhileRunning(t *testing.T) {
	// A stop op whose zonal operation reads DONE while the instance is still
	// observed RUNNING must NOT succeed — the create predicate (running =>
	// success) would be a false terminal for a stop.
	m, g := newGCEMock(t)
	m.instStatus.Store("RUNNING")
	data, _ := json.Marshal(opData{V: 1, Kind: "stop", Op: "op-stop-1", Zone: testZone, Instance: "ci-1"})
	status, err := g.ObserveOperation(context.Background(), provider.OperationRef{
		ActionID: "op_01PARK", Ref: ci1Ref(), Data: data,
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != provider.OpRunning || status.RetryAfter <= 0 {
		t.Fatalf("stop observed while RUNNING = %+v, want running (never success)", status)
	}
}

func TestGCPObserveStart_NotSuccessWhileStopped(t *testing.T) {
	m, g := newGCEMock(t)
	m.instStatus.Store("TERMINATED")
	data, _ := json.Marshal(opData{V: 1, Kind: "start", Op: "op-start-1", Zone: testZone, Instance: "ci-1"})
	status, err := g.ObserveOperation(context.Background(), provider.OperationRef{
		ActionID: "op_01PARK", Ref: ci1Ref(), Data: data,
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != provider.OpRunning {
		t.Fatalf("start observed while TERMINATED = %+v, want running (never success)", status)
	}
}

func TestGCPObserveOperation_UnknownKindIsOpUnknown(t *testing.T) {
	// A discriminator this binary does not understand (newer journal record)
	// must degrade to OpUnknown — never borrow another kind's predicate.
	_, g := newGCEMock(t) // instance observed RUNNING: create's predicate WOULD fire
	data, _ := json.Marshal(map[string]any{"v": 1, "kind": "suspend", "zone": testZone, "instance": "ci-1"})
	status, err := g.ObserveOperation(context.Background(), provider.OperationRef{
		ActionID: "op_01PARK", Ref: ci1Ref(), Data: data,
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != provider.OpUnknown {
		t.Fatalf("unknown op kind = %+v, want OpUnknown (never false success)", status)
	}
}

// --- vanished / rejected ---

func TestGCPStopStart_VanishedIsNotFound(t *testing.T) {
	for _, kind := range []string{"stop", "start"} {
		t.Run(kind, func(t *testing.T) {
			m, g := newGCEMock(t)
			m.instanceGone.Store(true)
			_, err := g.Apply(context.Background(), parkAction(kind))
			if !provider.IsClass(err, provider.ErrNotFound) {
				t.Fatalf("Apply(%s) on vanished = %v, want ErrNotFound", kind, err)
			}
			if provider.Effect(err) != provider.EffectNone {
				t.Fatalf("side effect = %v, want none", provider.Effect(err))
			}
		})
	}
}

func TestGCPObserveStopStart_VanishedIsNotFound(t *testing.T) {
	// A machine that vanishes mid-stop/start fails the op with ErrNotFound
	// (docs/12 §7): the discovery sweep owns the disposition.
	m, g := newGCEMock(t)
	m.instanceGone.Store(true)
	data, _ := json.Marshal(opData{V: 1, Kind: "stop", Op: "op-stop-1", Zone: testZone, Instance: "ci-1"})
	_, err := g.ObserveOperation(context.Background(), provider.OperationRef{
		ActionID: "op_01PARK", Ref: ci1Ref(), Data: data,
	})
	if !provider.IsClass(err, provider.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGCPStopLocalSSD400IsInvalidEffectNone(t *testing.T) {
	// GCE rejects stopping an instance with a local SSD unless
	// discardLocalSsd is set: 400 => ErrInvalid/EffectNone — a clean revert
	// (parking -> ready), never a retry loop or uncertainty resolution.
	m, g := newGCEMock(t)
	m.stop400.Store(true)
	_, err := g.Apply(context.Background(), parkAction("stop"))
	if !provider.IsClass(err, provider.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if provider.Effect(err) != provider.EffectNone {
		t.Fatalf("side effect = %v, want none (clean revert)", provider.Effect(err))
	}
	if m.stopCalls.Load() != 0 {
		t.Fatalf("stop calls = %d, want 0", m.stopCalls.Load())
	}
}

// --- the idempotency race (docs/12 §3: "in EVERY observable state
// combination") ---

func TestGCPStop_RaceToStoppedIsSuccess(t *testing.T) {
	// The dedup read sees RUNNING; another actor stops the instance before
	// our mutation lands, and GCE rejects the now-redundant stop with 400.
	// The driver must resolve the race by re-reading: settled => the
	// idempotent no-op success — NEVER ErrInvalid (which would revert
	// parking->ready while the machine is in fact stopped).
	m, g := newGCEMock(t)
	m.stop400.Store(true)
	m.raceTo.Store("TERMINATED")
	opRef, err := g.Apply(context.Background(), parkAction("stop"))
	if err != nil {
		t.Fatalf("raced stop of stopped must succeed (docs/12 §3): %v", err)
	}
	var data opData
	if err := json.Unmarshal(opRef.Data, &data); err != nil || data.Kind != "stop" || data.Op != "" {
		t.Fatalf("raced stop Data must carry the kind and NO zonal op: %s (err %v)", opRef.Data, err)
	}
	status, err := g.ObserveOperation(context.Background(), opRef)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != provider.OpSucceeded || status.Resource == nil || status.Resource.Phase != provider.PhaseStopped {
		t.Fatalf("status = %+v, want succeeded/stopped", status)
	}
}

func TestGCPStart_RaceToRunningIsSuccess(t *testing.T) {
	// Symmetric: the dedup read sees TERMINATED, another actor starts the
	// instance, GCE 400s the redundant start => re-read RUNNING => success.
	m, g := newGCEMock(t)
	m.instStatus.Store("TERMINATED")
	m.start400.Store(true)
	m.raceTo.Store("RUNNING")
	opRef, err := g.Apply(context.Background(), parkAction("start"))
	if err != nil {
		t.Fatalf("raced start of running must succeed (docs/12 §3): %v", err)
	}
	var data opData
	if err := json.Unmarshal(opRef.Data, &data); err != nil || data.Kind != "start" || data.Op != "" {
		t.Fatalf("raced start Data must carry the kind and NO zonal op: %s (err %v)", opRef.Data, err)
	}
	status, err := g.ObserveOperation(context.Background(), opRef)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != provider.OpSucceeded || status.Resource == nil || status.Resource.Phase != provider.PhaseRunning {
		t.Fatalf("status = %+v, want succeeded/running", status)
	}
}

func TestGCPStart_400WithoutStateChangeStaysInvalid(t *testing.T) {
	// A 400 with the instance still in the pre-mutation state is a GENUINE
	// rejection, not a race: the re-read is not settled, so the clean
	// ErrInvalid/EffectNone revert stands (never a false success).
	m, g := newGCEMock(t)
	m.instStatus.Store("TERMINATED")
	m.start400.Store(true) // raceTo unset: state stays TERMINATED
	_, err := g.Apply(context.Background(), parkAction("start"))
	if !provider.IsClass(err, provider.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if provider.Effect(err) != provider.EffectNone {
		t.Fatalf("side effect = %v, want none (clean revert)", provider.Effect(err))
	}
}

func TestGCPStopStart_RequireRef(t *testing.T) {
	_, g := newGCEMock(t)
	for _, kind := range []string{"stop", "start"} {
		_, err := g.Apply(context.Background(), provider.Action{ActionID: "op_01PARK", Kind: kind})
		if !provider.IsClass(err, provider.ErrInvalid) || provider.Effect(err) != provider.EffectNone {
			t.Fatalf("Apply(%s) without ref = %v, want ErrInvalid/EffectNone", kind, err)
		}
	}
}
