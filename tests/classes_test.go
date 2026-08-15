package tests

// Dynamic classes: seeding semantics, API-class CRUD, delete gates, and an
// acquisition provisioned from an API-created class with its policies live.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/samishal1998/fleetplane/internal/app"
	"github.com/samishal1998/fleetplane/internal/reconcile"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/providers/fake"
)

func TestClassSeedingConfigAuthoritative(t *testing.T) {
	h := newHarness(t, fake.Options{})
	ctx := context.Background()

	// The harness seeded its config classes; they must be config-sourced.
	rec, err := h.St.Classes().Get(ctx, "ci")
	if err != nil || rec.Source != "config" {
		t.Fatalf("seeded class: %+v, %v", rec, err)
	}
	// Re-seeding WITHOUT one class prunes it; api classes survive.
	if _, err := h.Svc.UpsertClass(ctx, app.UpsertClassCmd{
		Name: "api-extra", Kind: "compute.machine", Provider: "fake-local",
		Template: json.RawMessage(machineSpec), Actor: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.SeedConfigClasses(ctx, h.St,
		map[string]reconcile.Class{"ci": {Kind: "compute.machine", Provider: "fake-local", Spec: json.RawMessage(machineSpec)}},
		h.Clock.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.St.Classes().Get(ctx, "ci-reclaim"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("stale config class not pruned: %v", err)
	}
	if rec, err := h.St.Classes().Get(ctx, "api-extra"); err != nil || rec.Source != "api" {
		t.Fatalf("api class must survive re-seeding: %+v, %v", rec, err)
	}
}

func TestClassCRUDAndGates(t *testing.T) {
	h := newHarness(t, fake.Options{})
	ctx := context.Background()

	// Bad template rejected at definition time (kind-registry validation).
	if _, err := h.Svc.UpsertClass(ctx, app.UpsertClassCmd{
		Name: "bad", Kind: "compute.machine", Provider: "fake-local",
		Template: json.RawMessage(`{"image":"name:x"}`), Actor: "test", // missing serverType
	}); err == nil {
		t.Fatal("invalid template accepted")
	}
	// Unknown provider rejected.
	if _, err := h.Svc.UpsertClass(ctx, app.UpsertClassCmd{
		Name: "bad2", Kind: "compute.machine", Provider: "nope",
		Template: json.RawMessage(machineSpec), Actor: "test",
	}); err == nil {
		t.Fatal("unknown provider accepted")
	}
	// Create + MustCreate conflict.
	if _, err := h.Svc.UpsertClass(ctx, app.UpsertClassCmd{
		Name: "burst", Kind: "compute.machine", Provider: "fake-local",
		Template: json.RawMessage(machineSpec), Actor: "test", MustCreate: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Svc.UpsertClass(ctx, app.UpsertClassCmd{
		Name: "burst", Kind: "compute.machine", Provider: "fake-local",
		Template: json.RawMessage(machineSpec), Actor: "test", MustCreate: true,
	}); !errors.Is(err, app.ErrClassExists) {
		t.Fatalf("want ErrClassExists, got %v", err)
	}
	// Config-owned classes are immutable through the service.
	if _, err := h.Svc.UpsertClass(ctx, app.UpsertClassCmd{
		Name: "ci", Kind: "compute.machine", Provider: "fake-local",
		Template: json.RawMessage(machineSpec), Actor: "test",
	}); !errors.Is(err, app.ErrClassConfigOwned) {
		t.Fatalf("want ErrClassConfigOwned, got %v", err)
	}
	if err := h.Svc.DeleteClass(ctx, "ci", "test"); !errors.Is(err, app.ErrClassConfigOwned) {
		t.Fatalf("config class delete: want ErrClassConfigOwned, got %v", err)
	}
	// A pool referencing the class blocks deletion.
	h.CreatePool("burst-pool", reconcile.PoolSpec{Class: "burst", Replicas: 0})
	if err := h.Svc.DeleteClass(ctx, "burst", "test"); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("pool-referenced delete: want conflict, got %v", err)
	}
}

func TestAcquireFromAPIClass(t *testing.T) {
	h := newHarness(t, fake.Options{})
	ctx := context.Background()

	// Class created at runtime, with live reclaim + queue policies.
	if _, err := h.Svc.UpsertClass(ctx, app.UpsertClassCmd{
		Name: "runtime", Kind: "compute.machine", Provider: "fake-local",
		Template:    json.RawMessage(machineSpec),
		ReclaimIdle: 10 * time.Minute, QueueMaxWait: 5 * time.Minute, Actor: "test",
	}); err != nil {
		t.Fatal(err)
	}
	a := h.Acquire(app.AcquireCmd{Class: "runtime", TTL: time.Minute})
	h.DriveAll()
	if got := h.acq(a).State; got != storage.AcqBound {
		t.Fatalf("acquisition from api class = %s", got)
	}
	// The class default maxWait was resolved into the stored request.
	acq := h.acq(a)
	var req struct {
		MaxWaitMs int64 `json:"maxWaitMs"`
	}
	_ = json.Unmarshal(acq.Constraints, &req)
	if req.MaxWaitMs != (5 * time.Minute).Milliseconds() {
		t.Fatalf("class queue default not resolved at accept: %d", req.MaxWaitMs)
	}
	// Reclaim policy applies to the machine created from it.
	if err := h.Svc.Release(ctx, string(a), "test"); err != nil {
		t.Fatal(err)
	}
	h.Clock.Advance(11 * time.Minute)
	if n, _ := h.Rec.ReclaimPoolless(ctx); n != 1 {
		t.Fatalf("api-class reclaim policy not applied, reclaimed %d", n)
	}
}
