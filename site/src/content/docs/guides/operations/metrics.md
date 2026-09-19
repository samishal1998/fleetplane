---
title: "Metrics and alerting"
description: "Prometheus metrics on the ops listener and the alerts worth paging on."
---

`GET /metrics` on the **ops listener** exposes the fleet gauges plus the
standard Go runtime collector. Fleet gauges are computed from storage on each
scrape — there is no event-driven gauge drift.

```yaml
# prometheus.yml — scrape from the same host (ops listener is loopback)
scrape_configs:
  - job_name: fleetplane
    static_configs:
      - targets: ["127.0.0.1:9090"]
```

| Metric | Labels | Meaning |
|---|---|---|
| `fleetplane_resources` | `provider`, `kind`, `phase` | Resources by provider, kind and phase (tombstoned excluded) |
| `fleetplane_leases_active` | — | Active leases |
| `fleetplane_operations` | `state` | Non-terminal operations by state; `state="uncertain"` is **always** exported, even at 0 |
| `fleetplane_provider_health_state` | `provider`, `state` | Provider health (1 = current state) |
| `fleetplane_pools` | — | Configured pools |

Two label vocabularies you will alert on:

- Resource phases: `unknown`, `provisioning`, `ready`, `allocated`,
  `parking`, `parked`, `starting`, `draining`, `deleting`, `failed`,
  `orphaned` (there is no `deleted` phase — deletion is a storage tombstone,
  [ADR-017](/fleetplane/developers/adr/adr-017-operation-states/)).
- Non-terminal operation states: `journaled`, `in_flight`,
  `external_accepted`, `verifying`, `uncertain`.

Only `uncertain` is guaranteed to exist as a series; the other operation
states appear only while such operations exist, so write alert expressions
that tolerate absent series.

## Cost-aware leasing metrics

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

## Parked-machines metrics

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

## Machines sitting in `parking` or `starting`

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

## What to alert on

```yaml
groups:
  - name: fleetplane
    rules:
      # The one page-worthy alert: an operation is frozen awaiting a human.
      - alert: FleetplaneUncertainOperations
        expr: fleetplane_operations{state="uncertain"} > 0
        for: 1m
        labels: {severity: page}
        annotations:
          summary: "Uncertain operations need manual :resolve"
          runbook: "docs/guides/operations.md#uncertain-operations"

      - alert: FleetplaneProviderUnavailable
        expr: fleetplane_provider_health_state{state="unavailable"} == 1
        for: 5m
        labels: {severity: page}

      - alert: FleetplaneProviderDegraded
        expr: fleetplane_provider_health_state{state="degraded"} == 1
        for: 15m
        labels: {severity: warn}

      # A machine vanished provider-side; discovery will tombstone after grace.
      - alert: FleetplaneOrphanedResources
        expr: sum(fleetplane_resources{phase="orphaned"}) > 0
        for: 10m
        labels: {severity: warn}

      - alert: FleetplaneFailedResources
        expr: sum(fleetplane_resources{phase="failed"}) > 0
        for: 10m
        labels: {severity: warn}

      - alert: FleetplaneUnready
        expr: up{job="fleetplane"} == 0
        for: 2m
        labels: {severity: page}
```

`fleetplane_operations{state="uncertain"} > 0` is the alert this system is
designed around: `uncertain` means the engine has exhausted automatic
verification and **wants an operator** — nothing will move that operation
except a manual `:resolve` (next section).
