---
title: "Acquiring capacity"
description: "Acquire, watch, list and release capacity with the CLI, the HTTP API and the Go client."
---

An acquisition is one request for capacity; the scheduler reuses an existing machine before it creates one. The semantics — exclusive vs. shared leases, TTLs, idempotency, acquisition states — are in [concepts → acquisitions and leases](/fleetplane/guides/concepts/#acquisitions-and-leases).

## Acquire

Acquire capacity: reuse an existing machine that satisfies the constraints, or create a
new one from `--class` (design docs, [08 §3](https://github.com/samishal1998/fleetplane/blob/main/docs/08_HETZNER_RUNNER_USE_CASE.md)).

| Flag | Default | Description |
|---|---|---|
| `--class` | — | Resource class (needed to create new capacity) |
| `--cpu` | `0` | Minimum CPUs |
| `--memory-mib` | `0` | Minimum memory in MiB |
| `--exclusive` | `false` | Whole-machine lease (implied when no constraints are given) |
| `--ttl` | — | Lease TTL, e.g. `90m` |
| `--max-wait` | — | Queue budget, e.g. `10m`: wait for existing capacity before scaling up (needs server >= v0.4) |
| `--idempotency-key` | — | Idempotency key |

`--cpu`/`--memory-mib` become the constraints body
`{"cpu":{"min":N},"memoryMiB":{"min":N}}`. The accept response reflects the journaled
state, so the CLI immediately re-fetches the acquisition and prints its current state.

```bash
fleetplane acquire --class ci-large --cpu 4 --ttl 90m
```

```text
acq_01J8FYKF7H2K5N8Q1T4V9X0CED -> res_01J8FYK2N9V1X4T7Q0C3E6H9SD
state: bound
```

When a new machine has to be provisioned first, the state is `pending` (resource
column `-`) or `provisioning` (already showing the ID of the machine being created
for you) — use `fleetplane watch` to wait for `bound`.

## Wait for it to bind

Poll an acquisition (`acq_…`) or operation (`op_…`) until it reaches a terminal state,
printing each state transition.

| Flag | Default | Description |
|---|---|---|
| `--interval` | `2s` | Poll interval |
| `--timeout` | `10m` | Give up after this long |

Success means `bound` for acquisitions and `succeeded` for operations. A terminal
failure (`failed`, `expired`, `released` / `failed`, `aborted`) or a timeout exits with
code 6. A queued acquisition (`fleetplane acquire --max-wait`) may legitimately stay
`pending` for up to its whole queue budget before force-scaling, so give `--timeout`
at least the `--max-wait` value plus provisioning headroom.

```bash
fleetplane watch acq_01J8FYKF7H2K5N8Q1T4V9X0CED --timeout 5m
```

```text
acq_01J8FYKF7H2K5N8Q1T4V9X0CED: provisioning
acq_01J8FYKF7H2K5N8Q1T4V9X0CED: bound -> res_01J8FYKB4C7E1H9N2Q5S8T0VXD (lease lease_01J8FYKJ2E5H8K1N4Q7T0V3XCSD)
```

```bash
fleetplane acquire --class ci-large -o json | jq -r .id | xargs fleetplane watch
```

## See who holds what

List acquisitions and the machines they hold — "who is holding what right now".

| Flag | Default | Description |
|---|---|---|
| `--state` | — | Only these states, repeatable |
| `--resource` | — | Only acquisitions holding this `res_…` ID |
| `--all` | `false` | Include the terminal states (`failed`, `released`, `expired`) |

```bash
fleetplane acquisitions
```

```text
ID                              STATE  CLASS     RESOURCE                        LEASE                              ACTOR  AGE
acq_01J8FYKF7H2K5N8Q1T4V9X0CED  bound  ci-large  res_01J8FYK2N9V1X4T7Q0C3E6H9SD  lease_01J8FYKJ2E5H8K1N4Q7T0V3XCSD  ci     12m4s
```

With no flags the listing covers the live states only — `pending`,
`provisioning`, `bound`. Acquisitions are never garbage collected, so an
unfiltered listing would be the control plane's entire history; `--all` (or an
explicit `--state`) is how you ask for it.

```bash
fleetplane acquisitions --resource res_01J8FYK2N9V1X4T7Q0C3E6H9SD   # who holds this machine
fleetplane acquisitions --state failed --state expired              # what did not get capacity
```

## Release

Release an acquisition (retry-safe — releasing twice is not an error).

```bash
fleetplane release acq_01J8FYKF7H2K5N8Q1T4V9X0CED
```

```text
acq_01J8FYKF7H2K5N8Q1T4V9X0CED: released
```

## Over HTTP

Acquire asks Fleetplane to *find or create* suitable capacity: it prefers an existing ready resource with enough free capacity, and scales on demand otherwise. A request needs a `class` and/or `constraints`; `kind` defaults to `compute.machine`; `quantity` must be 1 in v1.

The request body also takes an optional `maxWait` (a Go duration string, e.g. `"10m"`): a queue budget letting the acquisition wait for existing capacity before scaling up ([cost-aware leasing](/fleetplane/guides/concepts/#cost-aware-leasing-billing-windows)); omitted, the class's `scheduling.queue.maxWait` applies. The field requires a server >= v0.4 — and because this endpoint rejects unknown fields, an older server answers a request carrying it with 400 `invalid` rather than silently ignoring it.

```bash
curl -sS -X POST "$BASE/v1/acquisitions" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: job-4211" \
  -d '{
    "class": "ci-large",
    "constraints": { "cpu": { "min": 2 }, "memoryMiB": { "min": 4096 } },
    "exclusive": true,
    "lease": { "ttl": "90m" }
  }'
```

The 201 body reflects the journaled state (usually `"state": "pending"`). Poll until it is `bound` — the response then carries `resourceId` and `leaseId`:

```bash
curl -sS -H "Authorization: Bearer $TOKEN" "$BASE/v1/acquisitions/acq_01J5X0B2M4T8RDWQ6YHF3KPVNC"
```

Terminal failure states are `failed` (no capacity and nothing to scale from) and `expired` (never satisfied within the pending timeout). When done, release — releasing twice is a no-op success:

```bash
curl -sS -X DELETE -H "Authorization: Bearer $TOKEN" \
  "$BASE/v1/acquisitions/acq_01J5X0B2M4T8RDWQ6YHF3KPVNC"
```

```json
{ "id": "acq_01J5X0B2M4T8RDWQ6YHF3KPVNC", "state": "released" }
```

The lease TTL is a safety net: if you crash before releasing, the lease expires on its own.

## Go client

`github.com/samishal1998/fleetplane/pkg/apiclient` is a minimal Go client over this API:

```go
package main

import (
	"context"
	"fmt"

	"github.com/samishal1998/fleetplane/pkg/apiclient"
)

func main() {
	c := apiclient.New("http://127.0.0.1:8080", "flp_1a2b3c4d.your-secret-here")
	acq, err := c.Acquire(context.Background(), apiclient.AcquireRequest{
		Class: "ci-large",
		Lease: &apiclient.LeaseRequest{TTL: "90m"},
	}, "job-4211")
	if err != nil {
		panic(err)
	}
	fmt.Println(acq.ID, acq.State)
}
```

Non-2xx responses come back as `*apiclient.APIError` carrying the decoded error detail.

## Queueing instead of scaling

`--max-wait` (or a class's `scheduling.queue.maxWait`) lets an acquisition wait for existing or in-flight capacity instead of scaling up immediately. See [parked machines and cost-aware leasing](/fleetplane/guides/parked-machines/#schedulingqueuemaxwait).
