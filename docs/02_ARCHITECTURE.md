# Architecture

## 1. High-level model

```text
                 +---------------------------+
                 | API / CLI / Other Callers |
                 +-------------+-------------+
                               |
                        Commands / Queries
                               |
                     +---------v---------+
                     |   Control Plane   |
                     |-------------------|
                     | Intent Service    |
                     | Resource Registry |
                     | Planner           |
                     | Scheduler         |
                     | Reconciler        |
                     | Operation Engine  |
                     | Lease Manager     |
                     +----+----------+---+
                          |          |
                    persistence      | provider calls
                          |          |
                  +-------v--+   +---v------------------+
                  | Storage  |   | Provider Registry   |
                  +----------+   +---+--------+---------+
                                     |        |
                                 Hetzner     GCP ...
                                     |
                              External Cloud API
```

Managed resources do **not** need a Fleetplane agent.

## 2. Layers

### Transport
HTTP API, CLI, optional gRPC later. Translates external requests into core commands.

### Application services
Implements acquire, release, ensure, inspect, reconcile, drain and destroy semantics.

### Control-plane kernel
Generic orchestration algorithms. No Hetzner/GCP/GitHub imports.

### Resource modules
Define higher-level resource semantics such as `compute.machine`, `database.postgres`, or a future `ci.runner`.

### Providers
Translate generic or module-specific operations into provider APIs.

### Persistence
Stores desired state, resource identity, leases, operation journal, observations and configuration metadata.

## 3. Registries

Fleetplane should use explicit registries:

- Provider registry
- Resource-kind registry
- Capability registry
- Policy registry
- Planner/action registry
- API extension registry (optional)

Compiled modules register during process initialization.

## 4. Resource identity

Every managed resource receives a Fleetplane identity independent of the provider ID:

```text
ResourceID: res_...
ProviderInstance: hetzner-prod
Kind: compute.machine
ExternalID: 12345678
Generation: 7
ObservedGeneration: 7
```

Provider tags/labels should include a stable Fleetplane ID where supported. This makes discovery and recovery safer.

## 5. Desired vs observed state

Fleetplane maintains:

- **Desired state**: what Fleetplane should make true.
- **Observed state**: last provider observation.
- **Allocation state**: who currently holds capacity.
- **Operation state**: mutations currently or previously attempted.

Observed state is cached, not blindly trusted forever. Reconciliation refreshes it from the provider.

## 6. Generic operation flow

```text
Request
  -> validate
  -> resolve resource kind/provider/policy
  -> observe candidate resources
  -> compute plan
  -> persist operation intent
  -> execute provider mutations
  -> poll/observe completion
  -> commit resulting state
  -> return resource/allocation
```

## 7. Why no universal cloud schema

Providers differ too much to normalize every field. Fleetplane should standardize only orchestration concepts and common capabilities.

A resource therefore has:

- common metadata;
- kind-specific spec/status;
- provider-specific opaque extension data;
- declared capabilities.

This avoids making advanced provider features impossible.

## 8. Concurrency model

The control plane may process many requests concurrently, but mutations should be serialized at the smallest safe scope:

- resource lock for single-resource mutations;
- pool lock for scale decisions;
- provider/account rate limiter;
- idempotency key for external commands.

SQLite v1 can use an in-process lock manager plus transactional state. A future HA backend requires distributed leases/locking.
