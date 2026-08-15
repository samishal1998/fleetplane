package tests

// The failure-injection catalog (doc 09, testing strategy) — one named test
// per scenario, each pinning the invariants it protects. The in-process
// crash tier uses Harness.Crash/Restart; the subprocess SIGKILL tier lives
// in subprocess_test.go.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/samishal1998/fleetplane/internal/app"
	"github.com/samishal1998/fleetplane/internal/operations"
	"github.com/samishal1998/fleetplane/internal/phase"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
	"github.com/samishal1998/fleetplane/providers/fake"
)

// FI-1: the provider accepted the create but the response was lost. The
// engine must resolve by the op label — never blind-retry into a duplicate
// (invariants 2, 7).
func TestFI_CreateAcceptedButResponseLost(t *testing.T) {
	h := newHarness(t, fake.Options{})
	h.Fake.AcceptButDropResponse()

	resID := h.Create("fi1")
	h.Drive(resID)

	if n := h.CloudObjects(); n != 1 {
		t.Fatalf("cloud has %d objects, want 1 (blind duplicate — invariant 7)", n)
	}
	res, err := h.St.Resources().Get(context.Background(), resID)
	if err != nil || res.Phase != phase.Ready || res.ExternalID == nil {
		t.Fatalf("resource not adopted to ready: %+v %v", res, err)
	}
}

// FI-2: crash after the journal write, before the provider call. The
// journaled state proves the provider was never called → exactly one safe
// re-dispatch (invariant 7's near-miss direction: retry IS allowed when
// provably unsent).
func TestFI_CrashAfterJournalBeforeProviderCall(t *testing.T) {
	h := newHarness(t, fake.Options{})
	resID := h.Create("fi2") // TxA committed; engine never ran
	h.Crash()
	h.Restart()
	h.Drive(resID)

	if n := h.CloudObjects(); n != 1 {
		t.Fatalf("recovery created %d objects, want exactly 1", n)
	}
	if h.Fake.ApplyCalls != 1 {
		t.Fatalf("Apply called %d times, want 1", h.Fake.ApplyCalls)
	}
	res, _ := h.St.Resources().Get(context.Background(), resID)
	if res.Phase != phase.Ready {
		t.Fatalf("phase = %s, want ready", res.Phase)
	}
}

// FI-3: crash after the provider accepted the create but before the ref
// committed (in_flight). Recovery adopts by op label (invariant 7).
func TestFI_CrashAfterProviderCreateBeforeCommit(t *testing.T) {
	h := newHarness(t, fake.Options{})
	armed := true
	h.Hooks = operations.Hooks{AfterApply: func(storage.OperationID) {
		if armed {
			panic("simulated crash between Apply and TxC")
		}
	}}
	h.open(h.Providers, fake.Options{}) // rebuild engine with hooks

	resID := h.Create("fi3")
	func() {
		defer func() { _ = recover() }()
		_, _ = h.Engine.Step(context.Background())
	}()
	if n := h.CloudObjects(); n != 1 {
		t.Fatalf("precondition: cloud object should exist after Apply, got %d", n)
	}
	op, err := h.opFor(resID)
	if err != nil || op.State != storage.OpInFlight {
		t.Fatalf("precondition: op = %v (%v), want in_flight", op, err)
	}

	armed = false
	h.Crash()
	h.Hooks = operations.Hooks{}
	h.Restart()
	h.Drive(resID)

	if n := h.CloudObjects(); n != 1 {
		t.Fatalf("recovery produced %d objects, want 1 — blind duplicate (invariant 7)", n)
	}
	res, _ := h.St.Resources().Get(context.Background(), resID)
	if res.Phase != phase.Ready || res.ExternalID == nil {
		t.Fatalf("adopted resource: %+v", res)
	}
}

// FI-4: delayed list consistency. The lost-response create is invisible to
// Discover for several list passes; the engine must keep verifying inside
// the window — never re-dispatch early (05 §10).
func TestFI_DelayedListConsistency(t *testing.T) {
	h := newHarness(t, fake.Options{ListLagSteps: 3})
	h.EngineCfg = operations.Config{VerifyWindow: time.Hour} // window >> lag
	h.open(h.Providers, fake.Options{})
	h.Fake.SetListLagSteps(3)
	h.Fake.AcceptButDropResponse()

	resID := h.Create("fi4")
	h.Drive(resID)

	if h.Fake.ApplyCalls != 1 {
		t.Fatalf("Apply called %d times — engine re-dispatched before the consistency window closed", h.Fake.ApplyCalls)
	}
	if n := h.CloudObjects(); n != 1 {
		t.Fatalf("cloud has %d objects, want 1", n)
	}
	res, _ := h.St.Resources().Get(context.Background(), resID)
	if res.Phase != phase.Ready {
		t.Fatalf("phase = %s, want ready (adopted after lag)", res.Phase)
	}
}

// FI-4b (near-miss): the create truly never happened. Past the verify
// window the engine re-dispatches EXACTLY once, with the same op id.
func TestFI_TrulyAbsent_RedispatchExactlyOnce(t *testing.T) {
	h := newHarness(t, fake.Options{})
	h.EngineCfg = operations.Config{VerifyWindow: 10 * time.Second}
	h.open(h.Providers, fake.Options{})
	// EffectMaybe failure that does NOT mutate: outcome uncertain, object
	// genuinely absent.
	h.Fake.FailNextApply(provider.ErrRetryable, provider.EffectMaybe, 1)

	resID := h.Create("fi4b")
	h.Drive(resID) // Drive advances the clock past the window

	if h.Fake.ApplyCalls != 2 { // 1 failed + exactly 1 re-dispatch
		t.Fatalf("Apply called %d times, want 2", h.Fake.ApplyCalls)
	}
	if n := h.CloudObjects(); n != 1 {
		t.Fatalf("cloud has %d objects, want 1", n)
	}
	op, err := h.St.Operations().Get(context.Background(), storage.OperationID(mustOpID(t, h, resID)))
	if err != nil || op.State != storage.OpSucceeded {
		t.Fatalf("op = %+v (%v), want succeeded", op, err)
	}
}

// FI-5: rate limiting. Retries must respect pacing (no thundering herd)
// and still converge.
func TestFI_RateLimitRespectsRetryAfter(t *testing.T) {
	h := newHarness(t, fake.Options{})
	h.Fake.RateLimitNext(2, 5*time.Second)

	resID := h.Create("fi5")

	// Without advancing the clock, repeated steps must NOT hammer Apply.
	before := h.Fake.ApplyCalls
	for i := 0; i < 5; i++ {
		if _, err := h.Engine.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	hammered := h.Fake.ApplyCalls - before
	if hammered > 1 {
		t.Fatalf("engine sent %d Apply calls without backoff elapsing", hammered)
	}

	h.Drive(resID) // advances the clock; converges
	res, _ := h.St.Resources().Get(context.Background(), resID)
	if res.Phase != phase.Ready {
		t.Fatalf("phase = %s, want ready after rate-limit storm", res.Phase)
	}
}

// FI-6: duplicate client retry with the same idempotency key — one side
// effect, replayed result (invariant 2 at the service layer).
func TestFI_DuplicateClientRetry(t *testing.T) {
	h := newHarness(t, fake.Options{})
	cmd := app.CreateResourceCmd{
		Kind: "compute.machine", Provider: "fake-local", Name: "fi6",
		Spec: json.RawMessage(machineSpec), Actor: "test",
		IdemKey: "retry-1", IdemScope: "test-scope", RequestHash: "h1",
		BuildResponse: func(r *storage.Resource) (int, json.RawMessage) {
			b, _ := json.Marshal(map[string]string{"id": string(r.ID)})
			return 201, b
		},
	}
	out1, err := h.Svc.CreateResource(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := h.Svc.CreateResource(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !out2.Replayed || string(out1.Body) != string(out2.Body) {
		t.Fatalf("replay mismatch: %v %s vs %s", out2.Replayed, out1.Body, out2.Body)
	}
	ops, _ := h.St.Operations().NonTerminal(context.Background())
	if len(ops) != 1 {
		t.Fatalf("%d journaled operations, want 1 (invariant 2)", len(ops))
	}
	// Near-miss: a different key with the same payload creates a second.
	cmd.IdemKey, cmd.RequestHash = "retry-2", "h1"
	if _, err := h.Svc.CreateResource(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	ops, _ = h.St.Operations().NonTerminal(context.Background())
	if len(ops) != 2 {
		t.Fatalf("distinct key produced %d ops, want 2", len(ops))
	}
}

// FI-7: delete of an already-deleted resource is terminal success — the
// tombstone lands, no error loop (ADR-017).
func TestFI_DeleteAlreadyDeleted(t *testing.T) {
	h := newHarness(t, fake.Options{})
	resID := h.Create("fi7")
	h.Drive(resID)

	// The machine vanishes out-of-band (console, another actor).
	res, _ := h.St.Resources().Get(context.Background(), resID)
	ref := provider.ExternalRef{ID: *res.ExternalID}
	if _, err := h.Fake.Apply(context.Background(), provider.Action{
		ActionID: "op_oob", Kind: "delete", Ref: &ref, Destructive: true,
	}); err != nil {
		t.Fatal(err)
	}

	out, err := h.Svc.DeleteResource(context.Background(), app.DeleteResourceCmd{
		ID: string(resID), Actor: "test",
		BuildResponse: func(r *storage.Resource) (int, json.RawMessage) {
			return 202, json.RawMessage(`{"ok":true}`)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = out
	applyBefore := h.Fake.ApplyCalls
	h.Drive(resID)

	res, _ = h.St.Resources().Get(context.Background(), resID)
	if res.DeletedAt == nil {
		t.Fatal("resource not tombstoned")
	}
	if h.Fake.ApplyCalls-applyBefore > 1 {
		t.Fatalf("delete-of-deleted looped: %d extra Apply calls", h.Fake.ApplyCalls-applyBefore)
	}
}

func mustOpID(t *testing.T, h *Harness, resID storage.ResourceID) string {
	t.Helper()
	// After Drive the op is terminal; find it via events is overkill —
	// scan all ops (test DBs are tiny).
	ops, err := h.St.Operations().Due(context.Background(), h.Clock.Now().UnixMilli()+1<<40, 1000)
	if err == nil {
		for _, op := range ops {
			if op.ResourceID != nil && *op.ResourceID == resID {
				return string(op.ID)
			}
		}
	}
	// Terminal ops aren't "due"; fall back to the resource's create event.
	evs, err := h.St.Events().List(context.Background(), storage.EventFilter{ResourceID: &resID}, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.OperationID != nil {
			return string(*ev.OperationID)
		}
	}
	t.Fatal("no operation found for resource")
	return ""
}
