---
title: "Parked machines and cost-aware leasing"
description: "Billing windows, acquisition queueing, two-stage reclaim and the parked warm tier: the knobs and the metrics that show they pay off."
---

Two features make idle capacity cheaper. **Cost-aware leasing** schedules against the provider's billing window rather than the lease; **parked machines** stop idle machines into a storage-price tier where the provider makes that economical. The concepts are in [concepts → cost-aware leasing](/fleetplane/guides/concepts/#cost-aware-leasing-billing-windows) and [concepts → parked machines](/fleetplane/guides/concepts/#parked-machines-the-warm-tier); this page collects the configuration, the operator commands and the metrics.

## Configuration

### Billing (`providers.<name>.billing`)

Optional per-kind overrides of the driver's billing policy — the input to
cost-aware leasing ([design doc 11](https://github.com/samishal1998/fleetplane/blob/main/docs/11_COST_AWARE_LEASING.md),
[ADR-018](/fleetplane/developers/adr/adr-018-cost-aware-leasing/); the behavior it enables is
explained in [concepts → cost-aware leasing](/fleetplane/guides/concepts/#cost-aware-leasing-billing-windows)).
`billing` is a map of **resource kind** → override; override kinds are
validated against the driver's declared kinds at boot, so a typo'd or
unserved kind is a boot error.

| Key | Type | Default | Behavior |
|---|---|---|---|
| `billing.<kind>.minimumDuration` | duration | driver's value | Shortest period the provider ever bills (`0` = none). Must be >= 0. |
| `billing.<kind>.increment` | duration | driver's value | Billing granularity after the minimum (`0` = fine-grained, per-use). Must be >= 0. |
| `billing.<kind>.terminationBuffer` | duration | driver's value | How long before a billing boundary a delete is dispatched so provider-side termination completes inside the paid window. Must be >= 0. |
| `billing.<kind>.adaptive` | bool | `false` | Derive the termination buffer from observed provider deletion durations (p95 + margin, floored by `terminationBuffer`, capped at half the increment). |
| `billing.<kind>.disabled` | bool | `false` | Explicit opt-out: the kind gets the zero (fine-grained) policy and no billing-window behavior. May not be combined with the other fields — that is a boot error. |

Overrides **merge field-wise** over the driver's declared policy: an unset
field keeps the driver's value, so a partial override never silently zeroes
the rest. The shipped driver defaults:

| Driver | Default billing policy |
|---|---|
| `hetzner` | Hourly increment + `5m` termination buffer for `compute.machine` |
| `digitalocean` | Hourly increment + `5m` termination buffer for `compute.machine` only |
| `aws` | `60s` minimum duration, no increment, for `compute.machine` only (per-second billing after the first minute) |
| `gcp` | `60s` minimum duration, no increment, for `compute.machine` only (per-second billing after the first minute) |
| `fake` | Whatever its `settings.billing` block declares (zero policy by default) |

A kind with the zero policy — no driver declaration, no override — gets
fine-grained billing: ordinary lease and idle reclamation, no billing-window
scheduling. Cost-aware behavior is therefore **off by default** and per
provider instance, per kind.

```yaml
providers:
  hetzner-main:
    driver: hetzner
    settings:
      token: secret://env/HETZNER_TOKEN
    billing:
      compute.machine:
        terminationBuffer: 10m   # partial override — increment stays hourly
        adaptive: true           # learn the buffer from observed deletes
```

### `reclaim.idleAfter`

Poolless resources created from a class with a reclaim policy are deleted
automatically once idle: every reconcile cycle, a class-created resource
that is `ready`, has **no active leases**, is not delete-protected, and has
been idle for at least `idleAfter` is drained and deleted (a journaled
delete, counted against the reconciler's mutation budget). Pool-managed
resources are sized by the pool instead. A class without `reclaim` is never
auto-reclaimed.

```yaml
classes:
  ci-large:
    kind: compute.machine
    provider: hetzner-main
    spec:
      serverType: cpx31
      image: "snapshot:ci-runner=v12"
    reclaim:
      idleAfter: 5m
```

### `reclaim.park` and `reclaim.deleteAfter` (two-stage reclaim)

On providers that can stop and resume machines, reclaim is two-stage
([concepts → parked machines](/fleetplane/guides/concepts/#parked-machines-the-warm-tier),
[design doc 12](https://github.com/samishal1998/fleetplane/blob/main/docs/12_PARKED_MACHINES.md)):

- **Stage 1** at `idleAfter`: if the provider is park-capable and `park` is
  not `never`, the idle machine is **parked** (a journaled stop — it drops to
  the storage-price tier and stays restartable in seconds) instead of
  deleted. On providers without the capability, or with `park: never`,
  behavior is exactly the plain delete above.
- **Stage 2** at `deleteAfter`, measured from when the machine entered
  `parked`: a machine parked that long is deleted. Unset keeps parked
  machines indefinitely — they are cheap, and the fleet keeps its warm tier.

Park capability is per driver and per kind: `gcp` and `aws` declare it for
`compute.machine` (stopped instances bill only disks/volumes and static
IPs); `hetzner` and `digitalocean` do **not** — those clouds bill powered-off
machines at full price, so delete-and-recreate stays optimal there and a
`park` policy is a no-op. The `fake` driver opts in via its `park` setting
([above](/fleetplane/guides/configuration/#driver-fake)). See the [providers guide](/fleetplane/guides/providers/overview/) for the
per-driver caveats.

`deleteAfter` is **poolless-only**: pool fleet size is owned by `replicas`
convergence, so a pool spec carrying `reclaim.deleteAfter` is rejected
(`400 invalid`). Pools opt into the warm tier through `spec.minRunning`
instead — keep at least that many machines hot, parking idle ones above the
floor and starting parked ones before creating when below it; `minRunning`
must be between 0 and `replicas`, and unset means no parking (today's
behavior). Pool specs are API objects, not config-file keys — see
[concepts → parked machines](/fleetplane/guides/concepts/#parked-machines-the-warm-tier).

```yaml
classes:
  ci-large:
    kind: compute.machine
    provider: gcp-main
    spec:
      serverType: e2-medium
      image: "family:debian-cloud/debian-12"
    reclaim:
      idleAfter: 10m     # stage 1: idle 10m -> parked (gcp is park-capable)
      park: auto         # default; "never" restores plain delete
      deleteAfter: 4h    # stage 2: parked 4h -> deleted; unset = parked forever
```

### `scheduling.queue.maxWait`

The class's cost/latency tradeoff for
[cost-aware leasing](/fleetplane/guides/concepts/#cost-aware-leasing-billing-windows): with
`maxWait` set, acquisitions of this class **wait for existing or in-flight
capacity** for up to this long before scaling up (sequential lease packing).
At the deadline the scheduler force-scales — a queued acquisition never waits
past its budget. `0` or absent means provision immediately (the previous
behavior). A per-request `maxWait` (API/CLI) overrides the class default.

Validation: must be >= 0 and must not exceed `acquire.pendingTimeout` — a
larger value is a boot error. The effective per-acquisition value is
additionally clamped to `pendingTimeout - 10s` at accept time (see
[acquire](/fleetplane/guides/configuration/#acquire)).

```yaml
classes:
  ci-large:
    kind: compute.machine
    provider: hetzner-main
    spec:
      serverType: cpx31
      image: "snapshot:ci-runner=v12"
    scheduling:
      queue:
        maxWait: 10m    # cost-oriented: pack work into already-paid windows
```

## Park and start by hand

### fleetplane resources park

Stop a ready machine into the near-free parked tier
([concepts → parked machines](/fleetplane/guides/concepts/#parked-machines-the-warm-tier)) —
`POST /v1/resources/{id}:park`. The stop is journaled and asynchronous —
follow the `resource.stop` operation with `fleetplane operations`, or poll
`fleetplane resources` until the phase is `parked`. On a provider that
cannot park (Hetzner, DigitalOcean) the request fails with 409 (exit code
5); a leased machine is refused the same way.

```bash
fleetplane resources park res_01J8FYK2N9V1X4T7Q0C3E6H9SD
```

```text
res_01J8FYK2N9V1X4T7Q0C3E6H9SD: parking
```

### fleetplane resources start

Start a parked machine back into service — `POST /v1/resources/{id}:start`.
The machine becomes `ready` again only after a fresh readiness probe, since
its public IP has usually changed across the stop/start cycle. Repeating
either command is safe: the API is idempotent and answers with the current
state instead of a conflict.

```bash
fleetplane resources start res_01J8FYK2N9V1X4T7Q0C3E6H9SD
```

```text
res_01J8FYK2N9V1X4T7Q0C3E6H9SD: starting
```

### Park and start over HTTP

`POST /v1/resources/{id}:park` stops a ready machine into the near-free
parked tier; `:start` brings a parked machine back into service
([concepts → parked machines](/fleetplane/guides/concepts/#parked-machines-the-warm-tier)).
Parking requires `resource.delete`; starting requires `resource.create`
(the route table above). Both are **idempotent at the API level** — a
retried request whose response was lost sees the current state, never a
spurious conflict:

| Status | When | Body |
|---|---|---|
| `202` | Fresh: the stop/start is journaled; the engine converges it in the background | `{"id": "res_…", "status": "parking"}` (or `"starting"`) |
| `200` | Already there: the resource is already in — or already moving to — the requested state | same shape |
| `409` | `park_unsupported`: the provider cannot park this kind. Or `conflict`: the resource is not in a state that can get there (e.g. parking a leased machine, starting a machine that is not parked) | error shape |

An operator's explicit `:park` deliberately bypasses the delete-protected
gate (protection guards deletion; parking is reversible) and the
queued-work gate; the automatic reclaim sweep respects both. Poll the
resource until its phase lands `parked` (or back at `ready` after a
`:start` — a failed stop reverts `parking → ready`, a failed start reverts
`starting → parked`).

## Is it paying off?

### Cost-aware leasing metrics

Nine series cover
[cost-aware leasing](/fleetplane/guides/concepts/#cost-aware-leasing-billing-windows)
([design doc 11](https://github.com/samishal1998/fleetplane/blob/main/docs/11_COST_AWARE_LEASING.md),
[ADR-018](/fleetplane/developers/adr/adr-018-cost-aware-leasing/)). Unlike the fleet gauges
they are event-driven and process-lifetime — they reset on restart, so use
`rate()`/`increase()` over them:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `fleetplane_resource_reuse_total` | counter | `class` | Acquisitions bound to already-existing capacity instead of a new create |
| `fleetplane_scale_up_avoided_total` | counter | `class` | Queued acquisitions that bound to existing capacity instead of scaling up |
| `fleetplane_acquisition_queue_seconds` | histogram | — | Time queued acquisitions waited before binding |
| `fleetplane_resource_termination_seconds` | histogram | `provider` | Delete operation duration, journal to provider-confirmed terminal |
| `fleetplane_billing_boundary_overruns_total` | counter | `provider` | Deletes of billing-aware resources confirmed after the boundary they targeted |
| `fleetplane_billing_window_missed_total` | counter | `provider` | Reclaim-eligible resources whose termination window passed unused (they wait a full extra increment) |
| `fleetplane_resource_paid_seconds_total` | counter | `provider`, `kind` | Billed lifetime of terminated billing-aware resources |
| `fleetplane_resource_useful_seconds_total` | counter | `provider`, `kind` | Leased (busy) time of terminated billing-aware resources — merged lease spans, not lease-seconds |
| `fleetplane_resource_paid_idle_seconds_total` | counter | `provider`, `kind` | Paid-but-idle time of terminated billing-aware resources (paid − useful, clamped at 0) |

Is the optimization working? Four signals: `paid_idle_seconds` should trend
**down** relative to `paid_seconds`, `scale_up_avoided_total` should be
**greater than 0** on queue-enabled classes, `boundary_overruns_total`
should stay **around 0** (if it climbs, raise the kind's `terminationBuffer`
or enable `adaptive`), and `window_missed_total` should stay **near 0**
(persistent misses mean the buffer and sweep cadence leave the termination
window practically unhittable).

### Parked-machines metrics

Three series cover
[parked machines](/fleetplane/guides/concepts/#parked-machines-the-warm-tier)
([design doc 12](https://github.com/samishal1998/fleetplane/blob/main/docs/12_PARKED_MACHINES.md),
[ADR-019](/fleetplane/developers/adr/adr-019-parked-machines/)). Like the cost metrics they
are event-driven and process-lifetime — use `rate()`/`increase()`:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `fleetplane_resource_park_total` | counter | `provider` | Machines parked (stopped into the storage-price tier) |
| `fleetplane_resource_start_total` | counter | `provider` | Parked machines started back into service |
| `fleetplane_resource_start_seconds` | histogram | `provider` | Start operation duration, journal to `ready` including the readiness probe — this histogram feeds the scheduler's queue-wait estimates |

The warm tier itself is visible in the existing fleet gauge:
`fleetplane_resources{phase="parked"}` (plus the transient `parking` and
`starting` phases).

### Machines sitting in `parking` or `starting`

`parking` and `starting` are operation-in-flight phases; a machine should
pass through them in seconds to a couple of minutes. If one lingers:

- **Find the operation**: `fleetplane operations` — the open
  `resource.stop` / `resource.start` operation names the resource and its
  attempt count.
- **Stop/start ops self-heal**: they are idempotent at the driver, so
  retries are plain re-dispatch — they never freeze `uncertain` for
  duplication reasons. Re-dispatch is **capped at 8 attempts**; past the
  cap the operation fails and the phase **reverts** (`parking → ready`,
  `starting → parked`), so a broken provider can never wedge a machine in a
  transitional phase. A failed start re-stamps `parkedAt`, so the machine is
  not immediately stage-2 deleted.
- **One asymmetric case**: a start that *succeeded* at the provider but
  exhausted the readiness probe lands `failed` — the machine is running at
  full price and unhealthy, so failed-cleanup deletes it rather than
  pretending it is parked.
- **Out-of-band stops are observation-only**: stopping a managed machine in
  the provider console does not move its Fleetplane phase to `parked` — the
  sweep records the stopped observation, nothing more (adoption of
  out-of-band stops is deferred, [ADR-019](/fleetplane/developers/adr/adr-019-parked-machines/) §10).
  Park through Fleetplane instead. The exceptions discovery does handle: an
  `orphaned` machine re-observed *stopped* revives to `parked`, and
  re-adopted stopped machines are minted `parked` on park-capable providers.
