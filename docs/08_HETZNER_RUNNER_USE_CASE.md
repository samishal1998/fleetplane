# Initial Use Case — Hetzner VM Fleet for CI Runners

## 1. Objective

Run Fleetplane continuously on a very small Hetzner VM. Create larger Hetzner Cloud servers from a prepared snapshot/image only when external callers request capacity.

The caller might be:
- a GitHub workflow;
- a CLI;
- a CI coordinator;
- Temporal;
- a custom service;
- a human.

Fleetplane does not depend on GitHub webhook semantics.

## 2. Example class

```yaml
classes:
  ci-large:
    kind: compute.machine
    provider: hetzner-main
    spec:
      serverType: cpx31
      image: "snapshot:ci-runner-v12"
      location: fsn1
      labels:
        fleetplane.pool: ci
```

## 3. Example imperative flow

```text
POST /v1/acquisitions
{
  "class": "ci-large",
  "quantity": 1,
  "lease": {"ttl": "90m"},
  "idempotencyKey": "ci:repo:run:job"
}
```

Fleetplane:

1. checks ready machines in the class;
2. checks whether any has allocatable capacity;
3. atomically reserves it if available;
4. otherwise creates a new server from the configured image;
5. waits until provider-level readiness criteria are met;
6. returns resource metadata;
7. caller uses the machine through its own integration;
8. caller releases the acquisition;
9. idle policy eventually deletes the machine.

## 4. Important distinction: machine readiness vs workload readiness

Because Fleetplane is agentless, provider-level `running` does not necessarily mean a GitHub runner inside the VM is registered and usable.

v1 options:
- caller handles workload readiness;
- class defines a TCP/HTTP readiness probe reachable from Fleetplane;
- provider/module supports cloud-init completion or another external readiness signal.

Do not hard-code GitHub runner registration into `compute.machine`.

A later `ci.runner` module could compose a machine resource with workload-specific readiness.

## 5. Declarative warm pool

```yaml
kind: Pool
metadata:
  name: ci
spec:
  resourceKind: compute.machine
  class: ci-large
  replicas: 2
  minReady: 1
  maxResources: 10
  reclaim:
    idleAfter: 5m
```

Changing `replicas` from 2 to 4 causes reconciliation to create two additional matching machines, accounting for machines already provisioning.

## 6. Hetzner implementation notes

The Hetzner Cloud API exposes server create/read/update/delete operations, server actions, server types, images/snapshots, labels and metrics. The provider should use stable labels to mark Fleetplane ownership and resource IDs.

The first provider should deliberately implement only the capabilities Fleetplane needs, then expand without bloating the generic core.

## 7. MVP demo

A convincing first demo:

```text
fleetplane acquire --class ci-large --cpu 2 --memory 8Gi
```

Output:
```text
acq_123 -> res_456
state: provisioning
provider: hetzner-main
```

Then:
```text
fleetplane watch acq_123
fleetplane release acq_123
fleetplane resources
```

The demo should visibly show reuse of existing capacity before creating another VM.
