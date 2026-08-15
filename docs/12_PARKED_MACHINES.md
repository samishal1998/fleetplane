# Parked Machines: Stop/Resume as a Warm Tier

## 1. Purpose

Some providers bill a stopped machine at a small fraction of its running
price. On GCP a `TERMINATED` instance incurs no compute charge — only its
disks and any static IP keep billing. On AWS a stopped instance bills only
its EBS volumes and Elastic IPs. Starting a stopped machine takes seconds
to about a minute; provisioning a fresh one takes minutes (boot, image,
readiness).

Other providers save nothing: Hetzner and DigitalOcean bill powered-off
machines at full price, so there the delete-and-recreate path (docs/11)
remains correct.

Fleetplane should therefore support a third lifecycle tier between
*running* and *deleted*:

```text
running   — full price, serving or ready to serve
parked    — storage-and-IP price, restartable in seconds
deleted   — free, recreate from scratch
```

— but only where the provider's economics make it real.

## 2. Core principle

Parking is a **provider capability plus a lifecycle policy**, exactly like
billing windows (docs/11 §20):

```text
Provider: "Can this kind stop and resume? What does stopped cost?
           How long does a start take?"
Fleetplane: "Given demand, policies and those facts, should this idle
             machine be parked, kept hot, or deleted — and should this
             acquisition start a parked machine instead of creating?"
```

A provider that cannot park never sees a stop action. A provider that can
park but whose operator disables it behaves exactly as today.

## 3. Provider capability

Conceptually:

```go
type ParkPolicy struct {
    Supported     bool
    StartEstimate time.Duration // typical stopped->running latency hint
}

type ParkAware interface {
    Parking(kind ResourceKind) ParkPolicy
}
```

Drivers implementing it also accept two new action kinds, `stop` and
`start`, with a hard contract:

1. **Both are idempotent**: stopping a stopped machine and starting a
   running machine return success. This is what makes crash recovery
   trivial (§7).
2. Stop preserves identity: labels/tags, the external ID, and disks
   survive; the kernel's ownership model is unaffected.
3. The driver reports honest observed phases while transitioning
   (`stopping…` → `stopped`; `starting…` → `running`).

Caveats the driver must surface, not hide: on GCP the ephemeral external
IP is released at stop — a restarted machine usually has a **new IP**, so
addresses must be re-observed and readiness re-probed after every start.
AWS instance-store-backed and spot instances cannot stop; the driver
rejects the action with an `invalid` error and the kernel falls back to
delete/create semantics for that machine.

## 4. Lifecycle

Three new resource phases:

```text
ready ──(journal stop)──▶ parking ──(stop confirmed)──▶ parked
parked ──(journal start)─▶ starting ──(running + probe)─▶ ready
parked ──(journal delete)─▶ deleting ─▶ tombstone
```

Rules:

- `ready → parking` requires the same gates as cost-based reclamation:
  zero active leases, managed ownership, and no queued compatible
  acquisition waiting for exactly this machine (parking what queued work
  needs would add a start latency for nothing).
- A failed stop reverts `parking → ready`; a failed start reverts
  `starting → parked` (evented; the caller falls back to creating).
- `parked` machines are **not scheduling candidates as-is** — they are
  candidates for *starting* (§6). They keep their identity labels and
  remain fully owned/discovered.
- Deleting a parked machine goes through the normal journaled delete with
  every existing gate.
- Explicit operator control exists alongside policy:
  `POST /v1/resources/{id}:park` and `:start`.

## 5. Reclaim becomes two-stage

Class (and pool) reclaim policy grows:

```yaml
reclaim:
  idleAfter: 10m      # stage 1 trigger, as today
  park: auto          # auto (default) | never
  deleteAfter: 4h     # stage 2: parked this long -> delete; unset = keep parked
```

- **Stage 1** at `idleAfter` (billing-window-timed when the provider has
  coarse increments, per docs/11): if the provider is park-capable and
  `park` is not `never` → journal a **stop**. Otherwise → journal a
  delete, exactly as today.
- **Stage 2**: a machine parked longer than `deleteAfter` is deleted
  (boundary-window rules apply to whatever residual billing the stopped
  machine still has — normally none, so deletion is immediate on the
  sweep). Unset `deleteAfter` keeps parked machines indefinitely: they are
  cheap, and the fleet keeps its warm tier.

## 6. Scheduler: start before create

The reuse ladder gains a rung:

```text
1. reserve an idle READY machine            (milliseconds)
2. start a PARKED compatible machine        (seconds to ~1 minute)
3. queue for capacity inside maxWait        (docs/11)
4. create a new machine                     (minutes)
```

Starting a parked machine reuses the scale-on-demand machinery: the
acquisition pre-binds to the machine (`pending_resource_id`), a start
operation is journaled with the `parked → starting` CAS (which also
serializes concurrent claimers — the CAS loser falls through the ladder),
and the existing op-terminal → reservation-bind path completes it.
Capacity is re-checked at bind, never assumed.

Parked machines are also **estimable** for docs/11 queue decisions: the
expected wait is the observed p50 start duration (fallback: the driver's
`StartEstimate`, then 60s) — usually far inside any `maxWait`, so queueing
beats creating whenever a parked machine exists.

## 7. Crash semantics

Stop and start are *idempotent* mutations, so they take the delete-shaped
path through the operation journal, not the create-shaped one:

- Crash with the op `journaled`: provably never sent — re-dispatch.
- Crash `in_flight`: re-dispatch the SAME action; idempotency makes the
  duplicate harmless. No discovery-based verification window is needed
  and stop/start ops can never enter `uncertain` for duplication reasons.
- A machine that vanishes mid-stop/start fails the op; the discovery sweep
  owns the orphan disposition as usual.

Invariant 7 is untouched: no stop/start ever creates anything.

## 8. Pools: a warm tier

```yaml
spec:
  replicas: 10      # total fleet, hot + parked
  minRunning: 2     # lower bound on hot capacity
  reclaim: { idleAfter: 10m, deleteAfter: 12h }
```

- `replicas` counts `provisioning + starting + ready + allocated +
  parking + parked` — a parked machine is still fleet.
- The reconciler keeps at least `minRunning` machines hot
  (ready/allocated/starting/provisioning), **starting parked machines
  before creating new ones** when below the floor, and parking idle
  machines above it per the reclaim policy.
- `minRunning` defaults to `replicas` when unset — i.e. pools behave
  exactly as today unless the operator opts into a warm tier.

## 9. Observability

```text
fleetplane_resources{phase="parked"}        (existing gauge, new phase)
fleetplane_resource_park_total{provider}
fleetplane_resource_start_total{provider}
fleetplane_resource_start_seconds{provider} (histogram — feeds §6 estimates)
```

Park/start operations appear in the operations list and events like any
journaled mutation. Docs/11 cost accounting treats parked time as neither
paid-compute nor useful; the conservative simplification is documented.

## 10. Invariants

1. A leased machine is never parked (extends invariant 3).
2. Parking never changes ownership, identity labels, or the external ID.
3. Stop/start actions are idempotent at the driver; a crash-duplicated
   dispatch is a no-op.
4. A parked machine is never a direct scheduling candidate; it becomes
   one only after a successful start **and** a fresh readiness probe
   (addresses may have changed).
5. Queued compatible work blocks parking the machine it waits for, same
   as it blocks deletion (docs/11 invariant 6).
6. On providers without the capability (or with `park: never`), behavior
   is byte-for-byte today's: nothing new is journaled.
7. Correctness and lease guarantees take precedence over cost (docs/11
   invariant 10).

## 11. Implementation sequence

Phase 1: SDK capability + phases (`parking`/`parked`/`starting`, schema
migration), engine stop/start op kinds with revert semantics, fake-driver
support, explicit `:park`/`:start` API + CLI + dashboard actions.

Phase 2: two-stage reclaim (`park`/`deleteAfter`), scheduler
start-before-create with pre-bind, queue estimation from start latency.

Phase 3: pool `minRunning` warm tier; GCP and AWS driver stop/start;
start-latency histogram feeding estimates.
