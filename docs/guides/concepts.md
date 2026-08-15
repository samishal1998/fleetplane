# Concepts

Fleetplane is a control plane for fleets of infrastructure resources — VMs first, but the kernel is generic. This guide explains the nouns you will see in every API response, CLI table, and dashboard view, and *why* the system is shaped the way it is: crash-safety and the no-duplicate-server guarantee drive almost every design decision below.

For full depth, see the [design docs](../00_README.md) (especially [02 Architecture](../02_ARCHITECTURE.md), [04 API and resource model](../04_API_AND_RESOURCE_MODEL.md), and [05 Reconciliation and scheduling](../05_RECONCILIATION_AND_SCHEDULING.md)). Everything described here is also visible in the built-in [web dashboard](dashboard.md), served by `fleetplane serve` at `http://<server.addr>/ui/`.

## The model at a glance

| Noun | ID prefix | What it is |
|---|---|---|
| Resource | `res_` | One piece of infrastructure Fleetplane tracks (a VM, a volume) |
| Class | — (config name) | A reusable creation template for a resource |
| Pool | `pool_` | Desired capacity: "keep N resources of this class alive" |
| Acquisition | `acq_` | One "give me capacity" request |
| Lease | `lease_` | Capacity of one resource allocated to one holder |
| Operation | `op_` | One journaled provider mutation (create/delete/stop/start) |
| Event | `evt_` | One append-only audit record |

All IDs are `<prefix>_<ULID>` ([ADR-004](../adr/ADR-004-ids.md)).

The one-sentence version: callers **acquire** capacity; the scheduler reuses an existing **resource** or creates one from a **class**; every provider mutation goes through the **operation journal** first; **pools** keep a desired number of resources converged in the background; **events** record everything.

## Resources and kinds

A resource is Fleetplane's record of one external thing. It has an identity independent of the provider's ID (`res_…` vs. the provider's numeric server ID), so discovery and crash recovery can re-match records to real infrastructure by label rather than by guesswork.

Two kinds ship today (registered in `pkg/kinds`):

| Kind | Capacity dimensions | Spec highlights |
|---|---|---|
| `compute.machine` | `cpu`, `memoryMiB` | `serverType`, `image` (required), `location`, `userData`, `labels`, `readiness` |
| `storage.volume` | `storageGiB` | `sizeGiB` (required), `zone`, `filesystem` |

The kernel never interprets kind- or provider-specific fields. Provider-native data rides along untouched in `status.extensions`, so advanced provider features are never erased (design principle: generic core, typed edges — see [02 Architecture §7](../02_ARCHITECTURE.md)).

```bash
fleetplane resources                 # ID  KIND  PROVIDER  PHASE  EXTERNAL  NAME
fleetplane resources get res_01ABC   # full JSON envelope
```

### The eleven phases

A resource's `status.phase` is Fleetplane's *orchestration* view — not the provider's native machine state (which is normalized separately and cached as an observation):

| Phase | Meaning |
|---|---|
| `unknown` | Recorded but not yet classified (e.g. freshly discovered) |
| `provisioning` | A create operation is journaled or executing at the provider |
| `ready` | Live and eligible for leases |
| `allocated` | At least one active lease holds its capacity |
| `parking` | A stop operation is journaled or executing (parked machines, below) |
| `parked` | Stopped at the provider — storage-price tier, restartable in seconds |
| `starting` | A start operation is journaled or executing |
| `draining` | No new leases; will be deleted once existing leases end |
| `deleting` | A delete operation is journaled or executing |
| `failed` | An operation failed terminally; the only exit is deletion |
| `orphaned` | The provider-side resource vanished; awaiting confirmed cleanup |

Transitions follow a fixed legal table (`internal/phase`): for example `draining → ready` is legal (undrain), `allocated → deleting` is not — a leased machine can never be deleted out from under its holder. `fleetplane resources delete` refuses while leases are active; `fleetplane resources drain` is the graceful path.

There is deliberately **no `deleted` phase**. Deletion terminality is a storage tombstone (`deletedAt` set), not a phase — so "is it gone?" has exactly one answer, and a crash between "phase says deleted" and "row removed" can never happen ([ADR-017](../adr/ADR-017-operation-states.md)).

## Classes

Classes can be defined two ways: statically in `config.yaml` (loaded at
boot, config-authoritative) or dynamically through `POST /v1/classes`, the
`fleetplane classes` CLI, `apply -f` Class manifests, or the dashboard.
Both kinds live in the same registry; config-defined classes show
`source: config` and are read-only through the API, while api-managed
classes take effect immediately — no restart. Templates are validated
against the kind registry at definition time.

A class is a creation template: everything needed to make a new resource of some kind at some provider. Classes live in the server config:

```yaml
classes:
  ci-large:
    kind: compute.machine
    provider: hetzner-main
    spec:
      serverType: cpx31
      image: "snapshot:ci-runner=v12"
      location: fsn1
    reclaim:
      idleAfter: 5m
```

Classes may contain provider-specific spec fields — they are templates, not portability promises ([04 §2](../04_API_AND_RESOURCE_MODEL.md)). `reclaim.idleAfter` gives class-created resources that belong to no pool an idle lifetime: once a machine has had no lease for that long, Fleetplane drains and deletes it automatically. That is what makes fire-and-forget `acquire` safe — released capacity does not leak money.

## Pools

A pool declares desired capacity and lets the reconciler converge toward it, continuously — `replicas: 4` means "keep four matching resources," not "create four once":

```yaml
apiVersion: fleetplane.io/v1alpha1
kind: Pool
metadata:
  name: ci-runners
spec:
  class: ci-large
  replicas: 4
  minReady: 1
  maxResources: 20
  reclaim:
    idleAfter: 10m
```

```bash
fleetplane apply -f pool.yaml        # create or update
fleetplane pools                     # list
fleetplane pools reconcile pool_01…  # force a cycle now
```

Reconciliation details that matter in practice:

- **Bounded mutations.** Each cycle performs at most `reconcile.maxMutationsPerCycle` creates/deletes (default 5). A typo'd `replicas: 400` becomes a slow, observable ramp instead of an instant stampede.
- **Graceful scale-down.** Surplus resources are *drained* (longest-idle first, never below `minReady`), then deleted once their leases end — never yanked.
- **`maxResources`** caps the pool's total footprint, counting resources still provisioning or draining.
- **Pause.** A paused pool is skipped by the reconciler entirely — its `paused` flag is visible on the pool envelope. Useful during incidents when you want the fleet frozen exactly as-is.
- **Failure backoff.** After provider failures, pool creates back off exponentially (base 5s, cap 10m) rather than hammering a broken API.

## Acquisitions and leases

An acquisition is one request for capacity: "give me a machine matching this class and/or these constraints, for this long." The contract ([05 §3–5](../05_RECONCILIATION_AND_SCHEDULING.md)) is **reuse before create**:

1. Rank existing `ready` resources with enough unallocated capacity (deterministic best-fit).
2. Atomically reserve one — the capacity check and the lease insert commit in a single transaction, so concurrent acquires can never over-allocate.
3. If nothing fits and a class was given, **scale on demand**: journal a create, pre-bind the acquisition to the pending resource, and bind it once the machine is ready.

```bash
# Whole machine from a class (exclusive is implied when no constraints given):
fleetplane acquire --class ci-large --ttl 90m --idempotency-key job-4711

# Shared capacity: 2 vCPU + 8 GiB carved out of any machine with room:
fleetplane acquire --cpu 2 --memory-mib 8192 --class ci-large --ttl 2h

# Wait for it to bind, then release when done:
fleetplane watch acq_01ABC --timeout 10m
fleetplane release acq_01ABC
```

Key semantics:

- **Exclusive vs. shared.** A shared lease takes a capacity slice (`{"cpu":{"min":2},"memoryMiB":{"min":8192}}`); multiple leases can share one machine. An exclusive lease (`--exclusive`, or implied when you pass no constraints) takes the whole resource and blocks all other holders.
- **Constraints without a class** can only reuse existing capacity — there is nothing to scale from, so an unsatisfiable request fails rather than hangs.
- **TTLs.** Leases expire automatically at `--ttl`; a background sweep (every 5s) releases expired leases, so a crashed caller cannot pin a machine forever. Acquisitions that never get satisfied expire after `acquire.pendingTimeout` (default 15m).
- **Idempotency.** Pass `--idempotency-key`; a network-level retry replays the stored response byte-for-byte instead of acquiring twice.

Acquisition states: `pending → provisioning → bound`, terminally `released`, `expired`, or `failed`. Lease states: `active`, `released`, `expired`.

## Cost-aware leasing (billing windows)

Leases, resources, and money have **three different lifetimes**. A lease is how long a caller needs capacity; a resource is the infrastructure providing it; the billing window is what the provider actually charges — often a full hour for a machine that ran ten minutes. Fleetplane keeps the three apart and schedules against the third ([design doc 11](../11_COST_AWARE_LEASING.md), [ADR-018](../adr/ADR-018-cost-aware-leasing.md)).

What that changes in practice:

- **Ending a lease does not destroy the machine.** Under coarse billing (hourly, say), a released machine's remaining window is already paid for. The scheduler prefers reusing that machine over creating a new one, and placement favors the candidate whose remaining paid window best covers the requested TTL.
- **Termination targets a window, not a moment.** An idle, reclaim-eligible machine is deleted inside a window just *before* its next billing boundary — early enough (the termination buffer) that provider-side deletion completes inside the increment already paid for. Past the point of no return — too close to the boundary for the delete to land in time — the next increment is inescapable, so Fleetplane deliberately keeps the machine and targets the following boundary: crossing a billing boundary intentionally beats paying for another hour and holding nothing.
- **Acquisitions can queue.** With a queue budget (`maxWait` on the request, or the class's `scheduling.queue.maxWait`), an acquisition waits for existing or in-flight capacity instead of scaling up immediately — two 5-minute jobs pack into one paid hour instead of two. At the deadline the scheduler force-scales, so a queued acquisition never waits past its budget. Queued work also *protects* capacity: an idle machine that a queued acquisition could use is not reclaimed out from under it (one queued request protects one best-fit machine, never the whole class).
- **Active leases always win.** Cost-based reclamation never touches a leased machine — correctness and lease guarantees take precedence over cost optimization.

Billing awareness is **per provider instance, per resource kind, and off by default**: it activates only where the driver declares a billing policy (Hetzner and DigitalOcean ship hourly defaults for machines) or where the config sets one — and `disabled: true` opts a kind back out. Providers with fine-grained billing keep the ordinary lease/idle reclamation path unchanged. The knobs live in the [configuration reference](configuration.md#billing-providersnamebilling); whether the optimization is actually paying off is measurable — see the [operations guide's cost metrics](operations.md#cost-aware-leasing-metrics).

## Parked machines (the warm tier)

Some providers bill a *stopped* machine at a small fraction of its running price: a stopped GCE instance incurs no compute charge (only its disks and static IPs keep billing), and a stopped EC2 instance bills only its EBS volumes and Elastic IPs. Starting a stopped machine takes seconds to about a minute; provisioning a fresh one takes minutes. Fleetplane models this as a third price tier between *running* and *deleted* ([design doc 12](../12_PARKED_MACHINES.md), [ADR-019](../adr/ADR-019-parked-machines.md)):

```text
running   — full price, serving or ready to serve
parked    — storage-and-IP price, restartable in seconds
deleted   — free, recreate from scratch
```

What that changes in practice:

- **Three new phases.** `ready → parking → parked` (a journaled stop) and `parked → starting → ready` (a journaled start plus a fresh readiness probe). Stop and start are idempotent driver actions, so crash recovery is plain re-dispatch — they never freeze `uncertain` for duplication reasons. A failed stop reverts `parking → ready`; a failed start reverts `starting → parked`. A parked machine keeps its identity labels, its external ID, and its ownership — it is still fleet, just cheap.
- **Reclaim becomes two-stage.** At `reclaim.idleAfter`, an idle machine on a park-capable provider is *parked* instead of deleted (opt out with `reclaim.park: never`); a second knob, `reclaim.deleteAfter`, deletes machines parked that long — unset keeps them parked indefinitely. `deleteAfter` is poolless-only: pool fleet size is owned by `replicas` convergence, so pool specs reject it. See the [configuration reference](configuration.md#reclaimpark-and-reclaimdeleteafter-two-stage-reclaim).
- **The scheduler ladder gains a rung.** Reuse before create becomes: reserve an idle `ready` machine (milliseconds) → **start a `parked` compatible machine** (seconds to ~a minute) → queue for capacity inside `maxWait` → create a new machine (minutes). Starting a parked machine pre-binds the acquisition and CASes `parked → starting`, which also serializes concurrent claimers — the loser falls through the ladder. Parked machines feed queue estimates too: the expected wait is the observed p50 start duration (falling back to the driver's estimate, then 60s), usually well inside any `maxWait`.
- **Pools get a warm tier.** `spec.minRunning` keeps at least that many machines hot (provisioning/starting/ready/allocated); idle machines above the floor are parked per the reclaim policy, and when the pool falls below the floor it **starts parked machines before creating new ones**. `replicas` counts hot + parked — a parked machine is still fleet. Unset `minRunning` means no parking: pools behave exactly as before unless you opt in.
- **It only exists where the economics are real.** Parking is a provider capability (GCP and AWS declare it for `compute.machine`; the fake driver opts in via settings). Hetzner and DigitalOcean bill powered-off machines at **full price**, so those drivers do not park — there the delete-and-recreate path above stays optimal, and nothing new is journaled.
- **The IP changes on restart.** GCP and AWS release the ephemeral public IP at stop, so a restarted machine usually has a **new address**. Fleetplane re-observes addresses and re-runs the readiness probe after every start — a parked machine becomes a scheduling candidate again only after both succeed. Don't cache pre-park addresses.

Operators can park and start explicitly — `POST /v1/resources/{id}:park` / `:start`, `fleetplane resources park`/`start`, or the dashboard's Park/Start buttons — alongside the automatic policy. The usual gates hold either way: a leased machine is never parked, and parking never changes ownership or identity. Whether the tier is earning its keep is measurable — see the [operations guide's parked-machines metrics](operations.md#parked-machines-metrics).

## The operation journal

This is the heart of the crash-safety story. **Every provider mutation is journaled before the provider is called** — the operation engine is the only component that talks to provider mutation APIs, and it is also the only retry authority ([ADR-014](../adr/ADR-014-retries.md)).

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

## Ownership, orphans, and ghosts

Ownership is Fleetplane's declared authority over a resource:

| Ownership | Fleetplane may mutate/delete? | Typical origin |
|---|---|---|
| `managed` | Yes | Created by Fleetplane, or re-adopted via our labels |
| `adopted` | Per explicit adoption policy | Deliberately brought under management |
| `observed` | No — read-only | Foreign/unlabeled infrastructure |

Fleetplane stamps everything it creates with reserved labels (`fleetplane.io/managed`, `fleetplane.io/owner`, `fleetplane.io/id`, `fleetplane.io/op`, `fleetplane.io/class` — [ADR-013](../adr/ADR-013-registration-labels.md)). Those labels are the recovery anchor: a periodic **discovery sweep** (`discovery.interval`, default 30s) compares provider reality against local records and resolves three mismatch cases ([ADR-017](../adr/ADR-017-operation-states.md)):

- **Orphan** (the `orphaned` phase): a local record whose provider resource vanished. It is tombstoned only after a second direct-Get confirmation, a `discovery.orphanGrace` grace window (default 60s), zero active leases, and a healthy provider — a cloud outage must never look like "everything was deleted."
- **Ghost** (a disposition, not a phase): a provider resource carrying our labels but bound to no live record — typically the loser of a duplicate-create race, or leftovers after a database restore. Handled per `discovery.ghostPolicy`: `delete` (default) issues a normal *journaled* delete; `surface` records it (`resource.ghost` event) and leaves cleanup to you. Never a blind inline delete, never a silent leak.
- **Re-adoption**: a labeled provider resource with no local record (e.g. you restored an older database) is re-adopted as `managed` — infrastructure must never become undeletable just because local state was lost ([06 Storage and HA §8](../06_STORAGE_AND_HA.md)).

Unlabeled resources are foreign. With `discovery.adoptUnlabeled: observed` they are recorded read-only for visibility; the default (`off`) ignores them. Fleetplane never mutates what it does not own.

## Events

Every mutation — API-triggered, reconciler-triggered, or discovery-triggered — appends an audit event: who (actor/token name), what (type, e.g. `acquisition.requested`, `acquisition.bound`, `resource.create`, `resource.drain`, `operation.succeeded`, `resource.orphaned`, `resource.ghost`, `pool.upserted`), against which resource/operation, with what outcome.

```bash
fleetplane events --since 1h           # recent history
fleetplane events --after evt_01ABC    # cursor-based tailing
```

Events are the answer to "what happened while I wasn't looking" — the dashboard's Overview and Events views are built on this stream.

## The dashboard

Everything above has a visual counterpart in the embedded web dashboard: fleet overview, resources with phase, pools with convergence status, acquisitions, the operation journal (including one-click resolution of `uncertain` operations), events, and provider health. It is served by the same `fleetplane serve` process at `http://<server.addr>/ui/` — no extra deployment. See the [dashboard guide](dashboard.md).

## Further reading

- [02 Architecture](../02_ARCHITECTURE.md) — layers, registries, desired vs. observed state
- [04 API and resource model](../04_API_AND_RESOURCE_MODEL.md) — envelopes, classes, pools, leases
- [05 Reconciliation and scheduling](../05_RECONCILIATION_AND_SCHEDULING.md) — the reconciliation equation, scheduler steps
- [06 Storage and HA](../06_STORAGE_AND_HA.md) — the journal protocol, SQLite, the HA path
- [11 Cost-aware leasing](../11_COST_AWARE_LEASING.md) — billing windows, queueing, termination policy
- [12 Parked machines](../12_PARKED_MACHINES.md) — the stop/resume warm tier
- [ADR-014](../adr/ADR-014-retries.md) — why the operation engine is the only retry authority
- [ADR-017](../adr/ADR-017-operation-states.md) — operation states, tombstone deletion, ghost vs. orphan
- [ADR-018](../adr/ADR-018-cost-aware-leasing.md) — the cost-aware leasing implementation decisions
- [ADR-019](../adr/ADR-019-parked-machines.md) — the parked-machines implementation decisions
