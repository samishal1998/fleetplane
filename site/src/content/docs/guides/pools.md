---
title: "Pools"
description: "Declare desired capacity and let the reconciler hold it: apply, inspect, reconcile, pause, resume and delete pools."
---

A pool declares desired capacity — "keep N resources of this class alive" — and the reconciler converges toward it continuously. The conceptual background (bounded mutations, graceful scale-down, failure backoff) is in [concepts → pools](/fleetplane/guides/concepts/#pools); this page is the how-to.

## Declare a pool

Declare desired fleet size and let the reconciler converge — via the declarative CLI:

```bash
fleetplane apply -f - <<'EOF'
apiVersion: fleetplane.io/v1alpha1
kind: Pool
metadata:
  name: ci-runners
spec:
  class: ci-large
  replicas: 3
EOF

fleetplane pools                       # list
fleetplane pools get pool_01J...       # inspect one
fleetplane pools reconcile pool_01J... # nudge reconciliation now
```

`fleetplane apply -f` accepts multi-document YAML (or JSON) with kinds `Pool` and `Resource`; re-applying the same file is idempotent (Resource applies are create-only, keyed by `metadata.name`). Equivalent imperative routes exist for automation: `POST /v1/pools` and `PUT /v1/pools/{id}` (permission `pool.write`), or `fleetplane pools apply -f pool.json` with a JSON manifest. Pool spec fields: `class`, `replicas`, plus optional `minReady`, `maxResources`, `reclaim.idleAfter`, or an inline `machine` spec with `provider` and `kind` instead of `class`.

## Manage pools from the CLI

List pools, or manage one with the subcommands.

```bash
fleetplane pools
```

```text
ID                               NAME     STATUS   SPEC
pool_01J8FYKN9Q2T5V8X1C4E7H0KSD  ci-warm  running  {"class":"ci-large","replicas":3}
```

`STATUS` is `running` or `paused` — see `pools pause` below.

### fleetplane pools apply

Create or update a pool from a JSON manifest (`-f` required). For YAML, use the
top-level `fleetplane apply` instead.

```json
{
  "apiVersion": "fleetplane.io/v1alpha1",
  "kind": "Pool",
  "metadata": {"name": "ci-warm"},
  "spec": {"class": "ci-large", "replicas": 3}
}
```

```bash
fleetplane pools apply -f pool.json
```

```text
ci-warm (pool_01J8FYKN9Q2T5V8X1C4E7H0KSD): applied
```

### fleetplane pools get

```bash
fleetplane pools get pool_01J8FYKN9Q2T5V8X1C4E7H0KSD
```

Prints the pool as JSON.

### fleetplane pools reconcile

Trigger reconciliation for one pool immediately instead of waiting for the periodic
reconciler (`reconcile.interval`, default `15s`).

```bash
fleetplane pools reconcile pool_01J8FYKN9Q2T5V8X1C4E7H0KSD
```

```text
pool_01J8FYKN9Q2T5V8X1C4E7H0KSD: reconciling
```

A paused pool refuses the kick with a 409 (exit code 5); resume it first.

### fleetplane pools pause / resume

Freeze convergence for one pool: while it is paused the reconciler makes no
creates, no drains, and no reclaim, so the fleet stays exactly as it stands —
what you want while investigating. `resume` lifts the freeze and reconciles
immediately.

```bash
fleetplane pools pause pool_01J8FYKN9Q2T5V8X1C4E7H0KSD
fleetplane pools resume pool_01J8FYKN9Q2T5V8X1C4E7H0KSD
```

```text
pool_01J8FYKN9Q2T5V8X1C4E7H0KSD: paused
pool_01J8FYKN9Q2T5V8X1C4E7H0KSD: running
```

Pausing is a verb, not a spec field: `fleetplane apply` never changes it, so a
manifest that says nothing about pausing cannot silently resume a pool you
paused ([ADR-API-002](/fleetplane/developers/adr/adr-api-002-operation-completeness/)).

### fleetplane pools delete

Remove a pool. Unlike a resource, a pool is deleted outright rather than
tombstoned — names are unique and meant to be reused. Two preconditions, each
a 409 (exit code 5): `spec.replicas` must be `0`, and no member resource may
still exist.

```bash
fleetplane pools apply -f pool-scaled-to-zero.json   # replicas: 0
fleetplane pools delete pool_01J8FYKN9Q2T5V8X1C4E7H0KSD
```

```text
pool_01J8FYKN9Q2T5V8X1C4E7H0KSD: deleted
```

Scale to `0` and let the members drain first; the pool's events stay in the
audit trail after it is gone.

## Pause, resume and delete over HTTP

`POST /v1/pools/{id}:pause` freezes convergence for one pool — no creates, no
drains, no reclaim — which is what an operator wants while investigating;
`:resume` lifts the freeze and kicks an immediate reconcile. Both are
synchronous, both take `pool.write`, and both answer `200` when the pool is
already in the requested state:

| Verb | Body |
|---|---|
| `POST /v1/pools/{id}:pause` | `{"id": "pool_…", "status": "paused"}` |
| `POST /v1/pools/{id}:resume` | `{"id": "pool_…", "status": "running"}` |

The flag is surfaced as the pool envelope's `paused` and is deliberately
**not** a manifest field: a bool has no "unset", so honouring it in `apply`
would mean a manifest that omits it silently resumed a paused pool
([ADR-API-002](/fleetplane/developers/adr/adr-api-002-operation-completeness/)). While a pool is
paused, `:reconcile` returns 409 `conflict` instead of accepting a kick the
paused reconciler would discard.

`DELETE /v1/pools/{id}` removes a pool. This is a hard delete, not the
resource tombstone — pool names are unique and operators reuse them. Two
preconditions, both re-checked inside the deleting transaction, both 409
`conflict`:

- `spec.replicas` must be `0`. A pool that still wants replicas is one the
  reconciler is actively creating into, and the in-flight resource would land
  on a pool row that no longer exists.
- No member resource may still exist. Scale to `0` and let the members drain
  first.

```json
{ "id": "pool_01J5X0C8T2VBKQ4WYNRD6HZMGS", "status": "deleted" }
```

The row is gone, so a second `DELETE` is `404`; the pool's events stay in the
audit trail.

## Create or update a pool over HTTP

`POST /v1/pools` upserts by `metadata.name` — posting an existing name updates that pool. `PUT /v1/pools/{id}` updates by ID. Both return 200 with the Pool envelope and kick the reconciler.

```bash
curl -sS -X POST "$BASE/v1/pools" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "kind": "Pool",
    "metadata": { "name": "ci-runners" },
    "spec": {
      "class": "ci-large",
      "replicas": 4,
      "minReady": 1,
      "maxResources": 20,
      "reclaim": { "idleAfter": "10m" }
    }
  }'
```

`replicas` is a convergence target, not a one-shot scale command — the reconciler continuously converges toward it. Trigger an immediate pass:

```bash
curl -sS -X POST -H "Authorization: Bearer $TOKEN" \
  "$BASE/v1/pools/pool_01J5X0C8T2VBKQ4WYNRD6HZMGS:reconcile"
```

```json
{ "id": "pool_01J5X0C8T2VBKQ4WYNRD6HZMGS", "status": "reconciling" }
```

A paused pool refuses the kick with `409` `conflict` — resume it first.

## Warm tier

On park-capable providers a pool can keep part of its fleet parked instead of running: `spec.minRunning` sets the hot floor. See [parked machines and cost-aware leasing](/fleetplane/guides/parked-machines/).
