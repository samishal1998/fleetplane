package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/samishal1998/fleetplane/internal/ids"
	"github.com/samishal1998/fleetplane/internal/phase"
	"github.com/samishal1998/fleetplane/internal/storage"
)

func newStore(t *testing.T) storage.Store {
	t.Helper()
	st, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "fp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func now() int64 { return time.Now().UnixMilli() }

func mkResource(t *testing.T, st storage.Store, ph phase.Phase) *storage.Resource {
	t.Helper()
	r := &storage.Resource{
		ID: storage.ResourceID(ids.New(ids.Resource)), Kind: "compute.machine",
		Provider: "fake-1", Ownership: storage.OwnershipManaged, Phase: ph,
		Spec:      json.RawMessage(`{"serverType":"cpx31","image":"snapshot:ci=1"}`),
		Extension: json.RawMessage(`{"native":{"zone":"fsn1","weird_field":[1,2,3]}}`),
		Capacity:  json.RawMessage(`{"cpu":4,"memoryMiB":8192}`),
		Labels:    map[string]string{"fleetplane.io/id": "x", "workload": "ci"},
		CreatedAt: now(), UpdatedAt: now(),
	}
	if err := st.Tx(context.Background(), func(tx storage.TxStore) error {
		return tx.Resources().Create(context.Background(), r)
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

func mkOp(res *storage.Resource) *storage.Operation {
	return &storage.Operation{
		ID: storage.OperationID(ids.New(ids.Operation)), Kind: storage.OpKindCreate,
		ResourceID: &res.ID, Provider: "fake-1", State: storage.OpJournaled,
		Action:    json.RawMessage(`{"actionId":"x","kind":"create"}`),
		CreatedAt: now(), UpdatedAt: now(),
	}
}

// --- TxA shape: idempotency claim + journal + event commit atomically ---

func TestTxA_JournalIdempotencyEventAtomic(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	res := mkResource(t, st, phase.Provisioning)
	op := mkOp(res)

	err := st.Tx(ctx, func(tx storage.TxStore) error {
		out, _, err := tx.Idempotency().Begin(ctx, "POST /v1/resources|ci", "key-1", "hash-1", op.ID)
		if err != nil {
			return err
		}
		if out != storage.BeginNew {
			t.Fatalf("Begin = %v, want BeginNew", out)
		}
		if err := tx.Operations().Append(ctx, op); err != nil {
			return err
		}
		return tx.Events().Append(ctx, &storage.Event{TS: now(), Type: "op.journaled", OperationID: &op.ID})
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.Operations().Get(ctx, op.ID)
	if err != nil || got.State != storage.OpJournaled {
		t.Fatalf("journaled op not persisted: %v %v", got, err)
	}

	// Rollback leaves nothing behind.
	op2 := mkOp(mkResource(t, st, phase.Provisioning))
	boom := errors.New("boom")
	err = st.Tx(ctx, func(tx storage.TxStore) error {
		if _, _, err := tx.Idempotency().Begin(ctx, "s", "key-2", "h", op2.ID); err != nil {
			return err
		}
		if err := tx.Operations().Append(ctx, op2); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, err := st.Operations().Get(ctx, op2.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("rolled-back operation is visible")
	}
	if out, _, _ := st.Idempotency().Begin(ctx, "s", "key-2", "h", op2.ID); out != storage.BeginNew {
		t.Fatalf("rolled-back idempotency key still claimed: %v", out)
	}
}

// --- one active operation per resource (journal admission guard) ---

func TestOperation_OneActivePerResource(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	res := mkResource(t, st, phase.Provisioning)

	op1, op2 := mkOp(res), mkOp(res)
	if err := st.Tx(ctx, func(tx storage.TxStore) error { return tx.Operations().Append(ctx, op1) }); err != nil {
		t.Fatal(err)
	}
	err := st.Tx(ctx, func(tx storage.TxStore) error { return tx.Operations().Append(ctx, op2) })
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("second active op on one resource: err = %v, want ErrConflict", err)
	}

	// Once op1 is terminal the slot frees.
	if err := st.Tx(ctx, func(tx storage.TxStore) error {
		if err := tx.Operations().Transition(ctx, op1.ID, storage.OpJournaled, storage.OpInFlight, nil); err != nil {
			return err
		}
		return tx.Operations().Transition(ctx, op1.ID, storage.OpInFlight, storage.OpSucceeded, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Tx(ctx, func(tx storage.TxStore) error { return tx.Operations().Append(ctx, op2) }); err != nil {
		t.Fatalf("slot did not free after terminal: %v", err)
	}
}

func TestOperation_TransitionCASAndTerminal(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	op := mkOp(mkResource(t, st, phase.Provisioning))
	if err := st.Tx(ctx, func(tx storage.TxStore) error { return tx.Operations().Append(ctx, op) }); err != nil {
		t.Fatal(err)
	}

	// Wrong from-state is ErrConflict.
	err := st.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Operations().Transition(ctx, op.ID, storage.OpInFlight, storage.OpSucceeded, nil)
	})
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("wrong-from transition: %v", err)
	}

	// Appending in a non-journaled state is refused.
	bad := mkOp(mkResource(t, st, phase.Provisioning))
	bad.State = storage.OpInFlight
	if err := st.Tx(ctx, func(tx storage.TxStore) error { return tx.Operations().Append(ctx, bad) }); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("Append(in_flight) accepted: %v", err)
	}

	// The verified path stamps attempt/refs via mut, terminal stamps terminal_at.
	if err := st.Tx(ctx, func(tx storage.TxStore) error {
		if err := tx.Operations().Transition(ctx, op.ID, storage.OpJournaled, storage.OpInFlight, func(o *storage.Operation) {
			o.Attempt++
			deadline := now() + 120_000
			o.VerifyDeadlineAt = &deadline
		}); err != nil {
			return err
		}
		return tx.Operations().Transition(ctx, op.ID, storage.OpInFlight, storage.OpExternalAccepted, func(o *storage.Operation) {
			o.ExternalRef = json.RawMessage(`{"id":"100001"}`)
		})
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Operations().Get(ctx, op.ID)
	if got.Attempt != 1 || got.VerifyDeadlineAt == nil || string(got.ExternalRef) != `{"id":"100001"}` {
		t.Fatalf("mut not persisted: %+v", got)
	}
	if got.TerminalAt != nil {
		t.Fatal("non-terminal op has terminal_at")
	}
	if err := st.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Operations().Transition(ctx, op.ID, storage.OpExternalAccepted, storage.OpSucceeded, nil)
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Operations().Get(ctx, op.ID)
	if got.TerminalAt == nil {
		t.Fatal("terminal op missing terminal_at")
	}
}

// --- invariant 2: completed idempotency keys replay stored bytes ---

func TestIdempotency_ReplayStoredBytes(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	scope, key := "POST /v1/acquisitions|ci", "workflow-123-job-build"
	stored := json.RawMessage(`{"id":"acq_1","state":"bound","weird":  [1,2]}`) // odd spacing on purpose

	op := mkOp(mkResource(t, st, phase.Provisioning))
	if err := st.Tx(ctx, func(tx storage.TxStore) error {
		if out, _, err := tx.Idempotency().Begin(ctx, scope, key, "h1", op.ID); err != nil || out != storage.BeginNew {
			t.Fatalf("first Begin: %v %v", out, err)
		}
		if err := tx.Operations().Append(ctx, op); err != nil {
			return err
		}
		// Concurrent duplicate while in progress → BeginInProgress.
		if out, rec, err := tx.Idempotency().Begin(ctx, scope, key, "h1", ""); err != nil || out != storage.BeginInProgress {
			t.Fatalf("in-progress Begin: %v %v %v", out, rec, err)
		}
		// Completion commits with the side effect's final state (R17).
		if err := tx.Operations().Transition(ctx, op.ID, storage.OpJournaled, storage.OpAborted, nil); err != nil {
			return err
		}
		return tx.Idempotency().Complete(ctx, scope, key, 201, stored, now()+48*3600*1000)
	}); err != nil {
		t.Fatal(err)
	}

	out, rec, err := st.Idempotency().Begin(ctx, scope, key, "h1", "")
	if err != nil || out != storage.BeginCompleted {
		t.Fatalf("replay Begin = %v, %v", out, err)
	}
	if rec.HTTPStatus != 201 || !bytes.Equal(rec.Result, stored) {
		t.Fatalf("replay is not byte-identical: %q vs %q", rec.Result, stored)
	}

	// Same key, different request → mismatch (409).
	if out, _, _ := st.Idempotency().Begin(ctx, scope, key, "OTHER-HASH", ""); out != storage.BeginMismatch {
		t.Fatalf("hash mismatch: %v, want BeginMismatch", out)
	}
	// Different key, same payload → genuinely new (invariant-2 near-miss).
	if out, _, _ := st.Idempotency().Begin(ctx, scope, "another-key", "h1", ""); out != storage.BeginNew {
		t.Fatal("distinct key was not treated as new")
	}
}

func TestIdempotency_GCSkipsNonTerminal(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	resA, resB := mkResource(t, st, phase.Provisioning), mkResource(t, st, phase.Provisioning)
	opTerminal, opOpen := mkOp(resA), mkOp(resB)

	if err := st.Tx(ctx, func(tx storage.TxStore) error {
		for _, o := range []*storage.Operation{opTerminal, opOpen} {
			if err := tx.Operations().Append(ctx, o); err != nil {
				return err
			}
		}
		if _, _, err := tx.Idempotency().Begin(ctx, "s", "k-done", "h", opTerminal.ID); err != nil {
			return err
		}
		if _, _, err := tx.Idempotency().Begin(ctx, "s", "k-open", "h", opOpen.ID); err != nil {
			return err
		}
		if err := tx.Operations().Transition(ctx, opTerminal.ID, storage.OpJournaled, storage.OpAborted, nil); err != nil {
			return err
		}
		expired := now() - 1000
		if err := tx.Idempotency().Complete(ctx, "s", "k-done", 200, json.RawMessage(`{}`), expired); err != nil {
			return err
		}
		return tx.Idempotency().Complete(ctx, "s", "k-open", 200, json.RawMessage(`{}`), expired)
	}); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := st.Tx(ctx, func(tx storage.TxStore) error {
		var err error
		n, err = tx.Idempotency().GC(ctx, now(), 100)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("GC removed %d, want exactly the terminal-op record", n)
	}
	if out, _, _ := st.Idempotency().Begin(ctx, "s", "k-open", "h", ""); out != storage.BeginCompleted {
		t.Fatal("record with non-terminal op was GCed (invariant 2 hole)")
	}
}

// --- invariant 6: provider bytes survive round trips exactly ---

func TestResource_ExtensionBytesRoundTrip(t *testing.T) {
	st := newStore(t)
	res := mkResource(t, st, phase.Ready)
	got, err := st.Resources().Get(context.Background(), res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Extension, res.Extension) {
		t.Fatalf("extension bytes mutated: %q vs %q", got.Extension, res.Extension)
	}
	if got.Labels["workload"] != "ci" {
		t.Fatalf("labels lost: %v", got.Labels)
	}
}

func TestResource_CASPhase(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	res := mkResource(t, st, phase.Provisioning)

	if err := st.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Resources().CASPhase(ctx, res.ID, phase.Provisioning, phase.Ready, now())
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Resources().Get(ctx, res.ID)
	if got.Phase != phase.Ready || got.ReadyAt == nil {
		t.Fatalf("CASPhase did not stamp ready_at: %+v", got)
	}

	err := st.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Resources().CASPhase(ctx, res.ID, phase.Provisioning, phase.Failed, now())
	})
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("stale CASPhase: %v, want ErrConflict", err)
	}
}

func TestResource_ExternalIDUniquePerProvider(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	a, b := mkResource(t, st, phase.Provisioning), mkResource(t, st, phase.Provisioning)

	if err := st.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Resources().SetExternalRef(ctx, a.ID, "ext-1", json.RawMessage(`{"id":"ext-1"}`))
	}); err != nil {
		t.Fatal(err)
	}
	err := st.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Resources().SetExternalRef(ctx, b.ID, "ext-1", json.RawMessage(`{"id":"ext-1"}`))
	})
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("duplicate external id accepted: %v", err)
	}

	// Tombstoning frees the identity for re-adoption (ADR-017).
	if err := st.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Resources().MarkDeleted(ctx, a.ID, now())
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Resources().SetExternalRef(ctx, b.ID, "ext-1", json.RawMessage(`{"id":"ext-1"}`))
	}); err != nil {
		t.Fatalf("external id still blocked after tombstone: %v", err)
	}
	// Tombstoned rows leave List by default.
	list, err := st.Resources().List(ctx, storage.ResourceFilter{Kind: "compute.machine"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range list {
		if r.ID == a.ID {
			t.Fatal("tombstoned resource still listed")
		}
	}
}

// --- invariant 1 backstop: exclusive partial unique index ---

func TestLease_ExclusiveIndexBackstop(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	res := mkResource(t, st, phase.Ready)
	acq := &storage.Acquisition{
		ID: storage.AcquisitionID(ids.New(ids.Acquisition)), Actor: "t", Kind: "compute.machine",
		Quantity: 1, State: storage.AcqPending, CreatedAt: now(), UpdatedAt: now(),
	}
	if err := st.Tx(ctx, func(tx storage.TxStore) error { return tx.Acquisitions().Insert(ctx, acq) }); err != nil {
		t.Fatal(err)
	}
	lease := func(exclusive bool) *storage.Lease {
		return &storage.Lease{
			ID: storage.LeaseID(ids.New(ids.Lease)), AcquisitionID: acq.ID, ResourceID: res.ID,
			Holder: "t", Capacity: json.RawMessage(`{"cpu":1}`), Exclusive: exclusive,
			State: storage.LeaseActive, CreatedAt: now(),
		}
	}
	first := lease(true)
	if err := st.Tx(ctx, func(tx storage.TxStore) error { return tx.Leases().Insert(ctx, first) }); err != nil {
		t.Fatal(err)
	}
	// Bypassing the kernel checks, the DB itself refuses a second active
	// exclusive lease (invariant 1 backstop).
	err := st.Tx(ctx, func(tx storage.TxStore) error { return tx.Leases().Insert(ctx, lease(true)) })
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("second exclusive active lease accepted: %v", err)
	}
	// Near-miss: after releasing, a new exclusive lease is legal.
	if err := st.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Leases().Transition(ctx, first.ID, storage.LeaseActive, storage.LeaseReleased, now())
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Tx(ctx, func(tx storage.TxStore) error { return tx.Leases().Insert(ctx, lease(true)) }); err != nil {
		t.Fatalf("exclusive lease after release refused: %v", err)
	}
}

func TestLease_SumActive(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	res := mkResource(t, st, phase.Ready)
	acq := &storage.Acquisition{
		ID: storage.AcquisitionID(ids.New(ids.Acquisition)), Actor: "t", Kind: "compute.machine",
		Quantity: 1, State: storage.AcqPending, CreatedAt: now(), UpdatedAt: now(),
	}
	l1 := &storage.Lease{ID: storage.LeaseID(ids.New(ids.Lease)), AcquisitionID: acq.ID, ResourceID: res.ID,
		Holder: "a", Capacity: json.RawMessage(`{"cpu":2,"memoryMiB":4096}`), State: storage.LeaseActive, CreatedAt: now()}
	l2 := &storage.Lease{ID: storage.LeaseID(ids.New(ids.Lease)), AcquisitionID: acq.ID, ResourceID: res.ID,
		Holder: "b", Capacity: json.RawMessage(`{"cpu":1}`), State: storage.LeaseActive, CreatedAt: now()}
	if err := st.Tx(ctx, func(tx storage.TxStore) error {
		if err := tx.Acquisitions().Insert(ctx, acq); err != nil {
			return err
		}
		if err := tx.Leases().Insert(ctx, l1); err != nil {
			return err
		}
		return tx.Leases().Insert(ctx, l2)
	}); err != nil {
		t.Fatal(err)
	}
	sum, err := st.Leases().SumActive(ctx, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sum["cpu"] != 3 || sum["memoryMiB"] != 4096 {
		t.Fatalf("SumActive = %v", sum)
	}
	if err := st.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Leases().Transition(ctx, l1.ID, storage.LeaseActive, storage.LeaseExpired, now())
	}); err != nil {
		t.Fatal(err)
	}
	sum, _ = st.Leases().SumActive(ctx, res.ID)
	if sum["cpu"] != 1 || sum["memoryMiB"] != 0 {
		t.Fatalf("SumActive after expiry = %v", sum)
	}
	got, _ := st.Leases().Get(ctx, l1.ID)
	if got.EndedAt == nil {
		t.Fatal("terminal lease missing ended_at")
	}
}

func TestAcquisition_BindAndResumeScan(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	res := mkResource(t, st, phase.Ready)
	acq := &storage.Acquisition{
		ID: storage.AcquisitionID(ids.New(ids.Acquisition)), Actor: "ci", Kind: "compute.machine",
		Class: "ci-large", Quantity: 1, TTLSeconds: 5400, State: storage.AcqPending,
		CreatedAt: now(), UpdatedAt: now(),
	}
	lease := &storage.Lease{ID: storage.LeaseID(ids.New(ids.Lease)), AcquisitionID: acq.ID, ResourceID: res.ID,
		Holder: "ci", Capacity: json.RawMessage(`{"cpu":2}`), State: storage.LeaseActive, CreatedAt: now()}

	if err := st.Tx(ctx, func(tx storage.TxStore) error {
		if err := tx.Acquisitions().Insert(ctx, acq); err != nil {
			return err
		}
		if err := tx.Acquisitions().Transition(ctx, acq.ID, storage.AcqPending, storage.AcqProvisioning, now()); err != nil {
			return err
		}
		if err := tx.Acquisitions().SetPendingResource(ctx, acq.ID, res.ID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The resume scan (plan R6) finds it.
	open, err := st.Acquisitions().ListByState(ctx, storage.AcqPending, storage.AcqProvisioning)
	if err != nil || len(open) != 1 || open[0].PendingResourceID == nil || *open[0].PendingResourceID != res.ID {
		t.Fatalf("resume scan: %v %v", open, err)
	}

	if err := st.Tx(ctx, func(tx storage.TxStore) error {
		if err := tx.Leases().Insert(ctx, lease); err != nil {
			return err
		}
		return tx.Acquisitions().Bind(ctx, acq.ID, lease.ID, now())
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Acquisitions().Get(ctx, acq.ID)
	if got.State != storage.AcqBound || got.LeaseID == nil || got.PendingResourceID != nil {
		t.Fatalf("bind result: %+v", got)
	}
	// Double bind is refused.
	err = st.Tx(ctx, func(tx storage.TxStore) error { return tx.Acquisitions().Bind(ctx, acq.ID, lease.ID, now()) })
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("double bind: %v", err)
	}
}

func TestEvents_CursorPagination(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	var idFirst storage.EventID
	if err := st.Tx(ctx, func(tx storage.TxStore) error {
		for i := 0; i < 3; i++ {
			ev := &storage.Event{TS: now(), Type: "test"}
			if err := tx.Events().Append(ctx, ev); err != nil {
				return err
			}
			if i == 0 {
				idFirst = ev.ID
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rest, err := st.Events().List(ctx, storage.EventFilter{After: idFirst}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 2 {
		t.Fatalf("cursor pagination returned %d, want 2", len(rest))
	}
	for _, ev := range rest {
		if ev.ID <= idFirst {
			t.Fatal("cursor ordering broken")
		}
	}
}

func TestCheckpointAndProviderInstanceRoundTrip(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Tx(ctx, func(tx storage.TxStore) error {
		if err := tx.Checkpoints().Put(ctx, "instance", json.RawMessage(`{"ownerId":"own_1"}`), now()); err != nil {
			return err
		}
		return tx.Providers().Upsert(ctx, &storage.ProviderInstanceRecord{
			Name: "hetzner-main", Driver: "hetzner",
			Config:  json.RawMessage(`{"credentials":{"token":"secret://env/HETZNER_TOKEN"}}`),
			Enabled: true, CreatedAt: now(), UpdatedAt: now(),
		})
	}); err != nil {
		t.Fatal(err)
	}
	cp, err := st.Checkpoints().Get(ctx, "instance")
	if err != nil || string(cp) != `{"ownerId":"own_1"}` {
		t.Fatalf("checkpoint: %s %v", cp, err)
	}
	p, err := st.Providers().Get(ctx, "hetzner-main")
	if err != nil || p.Driver != "hetzner" || !p.Enabled {
		t.Fatalf("provider instance: %+v %v", p, err)
	}
}
