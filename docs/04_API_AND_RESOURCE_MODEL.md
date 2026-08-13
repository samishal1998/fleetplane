# API and Resource Model

## 1. Core resource envelope

```yaml
apiVersion: fleetplane.io/v1alpha1
kind: Resource
metadata:
  id: res_01...
  name: runner-42
  labels:
    workload: ci
spec:
  kind: compute.machine
  provider: hetzner-prod
  class: runner-large
  constraints:
    cpu:
      min: 2
    memoryMiB:
      min: 8192
status:
  phase: ready
  externalRef:
    id: "123456"
  capacity:
    cpu: 4
    memoryMiB: 16384
  allocations:
    cpu: 2
    memoryMiB: 8192
```

## 2. Resource classes

Classes are reusable templates, not provider-independent promises.

```yaml
classes:
  runner-large:
    kind: compute.machine
    provider: hetzner-prod
    spec:
      image: github-runner-v7
      serverType: cpx31
      location: fsn1
```

A class may contain provider-specific fields. Generic callers can instead request constraints.

## 3. Imperative API

Primary operations:

```text
POST   /v1/acquisitions
DELETE /v1/acquisitions/{id}
POST   /v1/resources
GET    /v1/resources
GET    /v1/resources/{id}
POST   /v1/resources/{id}:drain
DELETE /v1/resources/{id}
POST   /v1/pools
PUT    /v1/pools/{id}
POST   /v1/pools/{id}:reconcile
GET    /v1/operations/{id}
GET    /v1/events
```

## 4. Acquire

An acquisition asks Fleetplane to find or create suitable capacity.

```json
{
  "kind": "compute.machine",
  "class": "runner-large",
  "quantity": 1,
  "constraints": {
    "cpu": {"min": 2},
    "memoryMiB": {"min": 8192}
  },
  "lease": {
    "ttl": "2h"
  },
  "idempotencyKey": "workflow-123-job-build"
}
```

The scheduler should first consider ready resources with sufficient unallocated capacity. If none exist and policy permits, it plans creation.

## 5. Pool

A pool expresses reusable desired capacity:

```yaml
kind: Pool
metadata:
  name: ci-runners
spec:
  resourceKind: compute.machine
  class: runner-large
  replicas: 4
  minReady: 1
  maxResources: 20
  reclaim:
    idleAfter: 10m
```

`replicas: 4` means reconciliation should converge toward four matching resources. It is not merely a one-shot scale command.

## 6. Lease/allocation

Leases separate resource existence from resource use.

```text
Resource: machine A, capacity 8 CPU / 16 GiB
Lease 1: 2 CPU / 8 GiB
Lease 2: 2 CPU / 4 GiB
Free:    4 CPU / 4 GiB
```

For resources that cannot safely host multiple allocations, the kind can advertise exclusive allocation.

## 7. Idempotency

Every externally triggered mutation should accept an idempotency key. The result of a completed request is replayable for a retention period.

This is essential when callers retry after network failure.

## 8. Declarative API

The same internal model can later accept manifests:

```yaml
apiVersion: fleetplane.io/v1alpha1
kind: Pool
metadata:
  name: build-fleet
spec:
  resourceKind: compute.machine
  class: runner-large
  replicas: 4
```

The declarative layer must call the same planner/reconciler as imperative commands rather than becoming a second orchestration engine.
