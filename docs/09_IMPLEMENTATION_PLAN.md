# Implementation Plan

## Phase 0 — ADRs and skeleton

Deliverables:
- repository;
- Go module;
- architecture decision records;
- core package boundaries;
- CI;
- lint/test;
- SQLite migration framework;
- module registry prototype.

Exit: binary starts, loads config, opens DB, exposes health API.

## Phase 1 — Generic resource kernel

Implement:
- Resource envelope;
- provider instances;
- resource-kind registry;
- capability model;
- observed state;
- operation journal;
- idempotency service;
- event log.

Exit: fake provider passes lifecycle tests.

## Phase 2 — Provider SDK + fake provider

Implement:
- SDK interfaces;
- typed error model;
- async operation model;
- conformance test kit;
- in-memory/fake provider.

Exit: create/get/discover/delete and crash-recovery tests work without a real cloud.

## Phase 3 — Hetzner compute provider

Implement:
- authentication;
- server discovery;
- server creation from image/snapshot;
- delete;
- power/status observation;
- server-type/image lookup;
- Fleetplane labels;
- pagination/rate-limit handling.

Exit: end-to-end VM lifecycle against a Hetzner test project.

## Phase 4 — Pools and reconciliation

Implement:
- Pool spec;
- replicas/minReady/max;
- periodic reconciliation;
- create/delete planning;
- draining;
- cooldown/backoff.

Exit: changing desired replicas converges without duplicate creates.

## Phase 5 — Acquisition and capacity scheduler

Implement:
- leases;
- capacity accounting;
- hard constraints;
- candidate scoring;
- atomic reservation;
- scale-on-demand;
- release;
- TTL expiry.

Exit: concurrent acquire calls cannot over-allocate the same capacity.

## Phase 6 — CLI/API hardening

Implement:
- acquire/release;
- resources/pools/operations/events;
- watch/poll;
- auth;
- structured errors;
- OpenAPI spec.

Exit: external automation can use Fleetplane without importing Go packages.

## Phase 7 — Production hardening

Implement:
- metrics;
- structured logs;
- graceful shutdown;
- backup/restore docs;
- migration safety;
- chaos/failure tests;
- provider API timeout/retry tuning;
- deletion safeguards.

## Phase 8 — Second provider

Implement GCP or DigitalOcean compute provider.

Purpose: prove abstractions are actually portable. Any core abstraction that only works because Hetzner behaves a certain way must be corrected here.

## Phase 9 — Non-VM proof

Implement one deliberately different resource, preferably a simple managed database or volume.

Purpose: validate that `Resource`, capabilities, planning and reconciliation are generic rather than VM-shaped.

## Suggested repository layout

```text
cmd/
  fleetplane/
  fleetplane-build/
internal/
  api/
  app/
  reconcile/
  scheduler/
  operations/
  storage/
pkg/
  sdk/
    provider/
    resource/
    capability/
    conformance/
  kinds/
    compute/
providers/
  hetzner/
  fake/
storage/
  sqlite/
  postgres/
docs/
examples/
```

## Testing strategy

### Unit
Planner, scheduler, capacity math, state machines.

### Property tests
Idempotency and reconciliation invariants.

### Provider contract
Reusable conformance suite.

### Integration
SQLite + fake provider.

### E2E
Real Hetzner project behind opt-in environment variables.

### Failure injection
- API timeout after provider accepted create;
- crash after journal write;
- crash after provider create but before local commit;
- delayed list consistency;
- rate limiting;
- duplicate client retry;
- delete of already-deleted resource.

## Critical invariants

1. A lease cannot allocate more capacity than a resource exposes.
2. A completed idempotency key cannot produce a second side effect.
3. A resource with an active lease cannot be reclaimed.
4. Reconciliation counts pending creates.
5. Fleetplane never deletes an unowned resource by default.
6. Provider-specific fields are not silently discarded.
7. Restarting the process cannot convert an uncertain operation into a blind duplicate mutation.
