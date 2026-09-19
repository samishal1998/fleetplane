---
title: "Architecture overview"
description: "The kernel, the operation-journal protocol and its crash-safety invariants, the reconciler, the scheduler and storage."
---

```text
            fleetplane CLI ──HTTP──▶ ┌──────────────────────────────┐
            web dashboard ──/ui/──▶  │  API  (auth, idempotency)    │
                                     ├──────────────────────────────┤
                                     │  scheduler   pool reconciler │
                                     │        operation engine      │◀── crash-safe journal
                                     ├──────────────────────────────┤
                                     │  SQLite (WAL)                │
                                     └──────┬───────────┬───────────┘
                                     provider SDK   provider SDK
                                          │               │
                                  Hetzner / DO      AWS / GCP APIs
```

The kernel (API, scheduler, reconciler, operation engine, storage) is provider-agnostic: it journals intent, dispatches through a narrow provider SDK, and verifies outcomes. Providers are thin adapters; the boundary is enforced by `scripts/check-boundaries.sh` and depguard, so orchestration code can never import a cloud SDK. Depth lives in the design docs: [architecture](https://github.com/samishal1998/fleetplane/blob/main/docs/02_ARCHITECTURE.md), [provider SDK](https://github.com/samishal1998/fleetplane/blob/main/docs/03_PROVIDER_SDK.md), [API and resource model](https://github.com/samishal1998/fleetplane/blob/main/docs/04_API_AND_RESOURCE_MODEL.md), [reconciliation and scheduling](https://github.com/samishal1998/fleetplane/blob/main/docs/05_RECONCILIATION_AND_SCHEDULING.md).

## The operation journal

This is the heart of the crash-safety story. **Every provider mutation is journaled before the provider is called** — the operation engine is the only component that talks to provider mutation APIs, and it is also the only retry authority ([ADR-014](/fleetplane/developers/adr/adr-014-retries/)).

Why journal first? Because the dangerous moment is the *crash window*: the process dies after sending "create server" but before recording the answer. Without a journal, restart cannot distinguish "never called" from "called, outcome unknown" — and the safe-looking fix (just create again) silently doubles your fleet and your bill. The journal makes that distinction a database fact:

| State | Meaning | Provider called? |
|---|---|---|
| `journaled` | Intent committed; nothing sent yet | Provably not |
| `in_flight` | Dispatch marker committed; call outcome unknown | Maybe |
| `external_accepted` | Provider assigned identity; external ref persisted | Yes |
| `verifying` | Post-crash or ambiguous transport; probing the provider | Being determined |
| `uncertain` | Verification exhausted; frozen for a human | Unknown |
| `succeeded` | Terminal success | Yes |
| `failed` | Terminal failure | Classified |
| `aborted` | Withdrawn before dispatch | No |

On restart, recovery is mechanical:

- `journaled` operations are provably un-sent → safe to dispatch.
- `in_flight` operations enter `verifying`: the engine searches the provider by the operation's own ID, which was stamped onto the request as the label `fleetplane.io/op`. Found → adopt that server as the result. Confirmed absent → exactly **one** re-dispatch with the *same* operation ID, so even a re-sent create deduplicates provider-side.
- Only when verification cannot decide within the verify window (`engine.verifyWindow`, default 120s) does the operation freeze as `uncertain`.

**`uncertain` means "a human must look."** The engine will never guess: it will neither blindly retry (possible duplicate) nor blindly mark failed (possible leak). Alert on it — `fleetplane_operations{state="uncertain"}` is always exported on `/metrics` — then resolve via the API or the dashboard:

```bash
fleetplane operations                          # list open operations
curl -X POST "$FLEETPLANE_ADDR/v1/operations/op_01ABC:resolve" \
  -H "Authorization: Bearer $FLEETPLANE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"action": "retry-verification"}'        # or "mark-failed"
```

(`:resolve` requires the `provider.admin` permission.)

The net guarantee: **restart never blind-duplicates infrastructure**. Repeated identical requests, crashes mid-create, and retried deletes all converge to exactly one server per intent.

### The journal protocol

Before a destructive or create mutation:

```text
1. transaction: record intended action
2. commit
3. call provider
4. persist external operation/reference
5. observe until terminal
6. persist result
```

After a crash, incomplete journal entries are resumed or reconciled.

### Journal state machine

`journaled → in_flight → external_accepted → succeeded | failed`, plus `verifying` (post-crash/ambiguous-transport), `uncertain` (frozen; manual `:resolve` only), `aborted` (from `journaled` only). All transitions are CAS on the from-state. The crash partition: `journaled` = provider provably never called (safe re-dispatch); `in_flight` = unknown (must verify before any re-mutation) — invariant 7 as schema.

## Crash-safety invariants

1. A lease cannot allocate more capacity than a resource exposes.
2. A completed idempotency key cannot produce a second side effect.
3. A resource with an active lease cannot be reclaimed.
4. Reconciliation counts pending creates.
5. Fleetplane never deletes an unowned resource by default.
6. Provider-specific fields are not silently discarded.
7. Restarting the process cannot convert an uncertain operation into a blind duplicate mutation.

Each invariant is pinned by tests: the fault-injection catalogue, the subprocess `kill -9` tier and the chaos suite all assert them — see [testing](/fleetplane/developers/testing/).

## One lifecycle, end to end

The walkthrough below is the scale-on-demand acquire path — the busiest single path through the system — followed by release and reclamation:

```mermaid
sequenceDiagram
    autonumber
    participant C as Caller
    participant A as API + scheduler
    participant J as SQLite (journal)
    participant E as Operation engine
    participant P as Provider

    C->>A: POST /v1/acquisitions (class ci-large, ttl 90m)
    A->>J: insert acquisition (pending) + audit event
    Note over A,J: no ready resource fits → scale on demand
    A->>J: journal resource.create (journaled), pre-bind acquisition
    A-->>C: acquisition acq_… accepted (caller polls / watches)
    E->>J: journaled → in_flight (CAS on state)
    E->>P: create server (label fleetplane.io/op = op_…)
    P-->>E: accepted, external id assigned
    E->>J: in_flight → external_accepted (ref persisted)
    E->>P: poll provider operation until terminal
    E->>J: op succeeded; resource provisioning → ready
    Note over J: lease activates; acquisition → bound; resource ready → allocated
    C->>A: DELETE /v1/acquisitions/acq_… (release)
    A->>J: lease released; resource allocated → ready
    Note over J: idle ≥ reclaim.idleAfter → ready → draining → deleting
    E->>J: journal resource.delete → in_flight → external_accepted
    E->>P: delete server
    E->>J: op succeeded; resource tombstoned (deletedAt set)
```

If the process crashes anywhere between steps 6 and 9, restart resumes from the journal: the engine verifies by the `fleetplane.io/op` label and either adopts the server the provider already made or safely re-dispatches the same operation — never both.

## Reconciler

For a desired pool:

```text
delta = desired_matching_resources - observed_matching_resources
```

But real reconciliation must account for:

- resources still provisioning;
- draining resources;
- failed resources;
- allocations;
- cooldowns;
- provider quota;
- pending create/delete operations;
- adoption policy;
- min-ready requirements.

Reconciliation details that matter in practice:

- **Bounded mutations.** Each cycle performs at most `reconcile.maxMutationsPerCycle` creates/deletes (default 5). A typo'd `replicas: 400` becomes a slow, observable ramp instead of an instant stampede.
- **Graceful scale-down.** Surplus resources are *drained* (longest-idle first, never below `minReady`), then deleted once their leases end — never yanked.
- **`maxResources`** caps the pool's total footprint, counting resources still provisioning or draining.
- **Pause.** A paused pool is skipped by the reconciler entirely — its `paused` flag is visible on the pool envelope. Useful during incidents when you want the fleet frozen exactly as-is.
- **Failure backoff.** After provider failures, pool creates back off exponentially (base 5s, cap 10m) rather than hammering a broken API.

## Scheduler

An acquisition is one request for capacity: "give me a machine matching this class and/or these constraints, for this long." The contract ([05 §3–5](https://github.com/samishal1998/fleetplane/blob/main/docs/05_RECONCILIATION_AND_SCHEDULING.md)) is **reuse before create**:

1. Rank existing `ready` resources with enough unallocated capacity (deterministic best-fit).
2. Atomically reserve one — the capacity check and the lease insert commit in a single transaction, so concurrent acquires can never over-allocate.
3. If nothing fits and a class was given, **scale on demand**: journal a create, pre-bind the acquisition to the pending resource, and bind it once the machine is ready.

With parked machines the ladder gains a rung:

- **The scheduler ladder gains a rung.** Reuse before create becomes: reserve an idle `ready` machine (milliseconds) → **start a `parked` compatible machine** (seconds to ~a minute) → queue for capacity inside `maxWait` → create a new machine (minutes). Starting a parked machine pre-binds the acquisition and CASes `parked → starting`, which also serializes concurrent claimers — the loser falls through the ladder. Parked machines feed queue estimates too: the expected wait is the observed p50 start duration (falling back to the driver's estimate, then 60s), usually well inside any `maxWait`.

Cost-aware leasing adds billing-window placement and queueing on top; the decisions are recorded in [ADR-018](/fleetplane/developers/adr/adr-018-cost-aware-leasing/) and [ADR-019](/fleetplane/developers/adr/adr-019-parked-machines/).

## Storage

SQLite is appropriate for the first single-control-plane deployment:

- no external service;
- transactional;
- easy backup;
- ideal for a tiny always-on node;
- sufficient for moderate control-plane write volume.

Use WAL mode and explicit migrations.

### Connection discipline

Two handles: a 1-connection writer (`_txlock=immediate`, `synchronous=FULL` — the operation journal must survive power loss) and an N-connection reader pool (`synchronous=NORMAL`), plus one dedicated backup connection for `VACUUM INTO`. WAL mode; PRAGMAs live only in the DSN (ADR-010).

Orchestration code never sees SQL. The root handle, from `internal/storage/store.go`:

```go
// Store is the root persistence handle (06 §2 + plan R23). Reads outside a
// transaction use the reader pool; Tx serializes writes on the single writer
// connection with BEGIN IMMEDIATE.
type Store interface {
	TxStore // autocommit reads

	// Tx runs fn in one write transaction (BEGIN IMMEDIATE). Nesting is a
	// programming error and panics.
	Tx(ctx context.Context, fn func(TxStore) error) error
	// View runs fn on a read-only snapshot.
	View(ctx context.Context, fn func(TxStore) error) error

	Ping(ctx context.Context) error
	// Backup writes a consistent compacted copy via VACUUM INTO on a
	// dedicated connection (never the writer) — plan R23.
	Backup(ctx context.Context, destPath string) error
	Close() error
}

// TxStore exposes the domain sub-stores, all operating in one context
// (a transaction inside Tx/View, autocommit otherwise).
type TxStore interface {
	Providers() ProviderInstanceStore
	Resources() ResourceStore
	Pools() PoolStore
	Classes() ClassStore
	Acquisitions() AcquisitionStore
	Leases() LeaseStore
	Operations() OperationStore
	Idempotency() IdempotencyStore
	Events() EventStore
	Checkpoints() CheckpointStore
}
```

Schema migrations are embedded goose migrations, forward-only, with a downgrade guard at boot ([ADR-010](/fleetplane/developers/adr/adr-010-migrations/)). The HA path beyond a single SQLite node is sketched in the [storage and HA design doc](https://github.com/samishal1998/fleetplane/blob/main/docs/06_STORAGE_AND_HA.md).

## Design documents

The original design docs live in the repository under [`docs/`](https://github.com/samishal1998/fleetplane/tree/main/docs) (`00_README.md` … `12_PARKED_MACHINES.md`); the [ADRs](/fleetplane/developers/adr/) record where the implementation deviates from them.
