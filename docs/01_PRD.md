# Fleetplane — Product Requirements Document

## 1. Summary

Fleetplane is an extensible control plane for managing fleets of infrastructure resources across heterogeneous providers.

The first release focuses on **agentless VM fleet management**. The control plane does not require a daemon on managed machines. Instead, providers inspect and mutate infrastructure using the provider's external API.

The primary initial scenario is a tiny, continuously running control-plane instance on Hetzner that can create larger machines from prepared images when an external caller requests capacity. A GitHub workflow may be one caller, but GitHub is not part of the core contract.

## 2. Problem

Cloud automation repeatedly needs the same primitives:

- create a resource only when capacity is needed;
- choose among existing resources before creating new ones;
- provision from a known image/template;
- track allocation and ownership;
- release or destroy idle capacity;
- maintain a requested minimum or exact fleet size;
- inspect actual provider state and repair drift;
- expose the same control logic through an API, CLI, workflow or scheduler;
- work across providers without forcing the control plane to understand every provider-specific resource.

Existing tools tend to optimize for either declarative infrastructure provisioning, container orchestration, or a specific workload. Fleetplane instead provides a reusable **control-plane kernel**.

## 3. Goals

### P0
- Agentless operation.
- Go implementation.
- Single self-contained binary for a selected set of compiled modules.
- Generic provider/resource interfaces.
- Imperative API as the primary interface.
- Basic declarative desired-state reconciliation.
- VM pool and capacity primitives.
- Hetzner Cloud provider.
- SQLite single-node persistence.
- Idempotent operations and crash recovery.
- API authentication.
- Audit/event history.
- CLI.
- Graceful scale-up and scale-down.
- Prepared-image VM creation.

### P1
- Generic leases/allocations.
- Resource constraints and placement.
- Policy-driven idle reclamation.
- Multiple provider instances/accounts.
- Postgres storage backend.
- Metrics and OpenTelemetry.
- Provider conformance test suite.
- GCP and AWS VM providers.

### P2
- HA control plane.
- External plugin protocol.
- Rich declarative DSL.
- Non-VM resources such as managed databases.
- Cost-aware scheduling.
- Spot/preemptible capacity.
- Multi-provider failover.

## 4. Non-goals for v1

- Replacing Terraform/OpenTofu.
- Container scheduling.
- Installing an agent on every managed VM.
- A GitHub Actions-specific control plane.
- General workflow orchestration.
- Arbitrary user code execution inside the control-plane process.
- Distributed consensus in the first release.
- Full cloud-resource schema normalization.

## 5. Users

### Infrastructure engineer
Wants an API that can request `2 vCPU / 8 GiB` capacity without writing provider-specific lifecycle code.

### Platform engineer
Defines pools and policies, then lets other systems acquire/release capacity.

### Provider/module author
Adds a provider or a higher-level resource implementation without modifying the orchestration kernel.

## 6. Product principles

1. **Imperative first, declarative compatible.**
2. **Generic core, typed edges.**
3. **Capabilities over universal lowest-common-denominator schemas.**
4. **Agentless by default.**
5. **Provider state is observable truth; local state is orchestration truth.**
6. **Every mutation is idempotent or protected by an idempotency mechanism.**
7. **Compiled modules first.**
8. **Single-node simplicity before distributed complexity.**
9. **Escape hatches are allowed; provider-specific features must not be erased.**
10. **Plan before mutate.**

## 7. Key user stories

- As a caller, I can request one resource matching a class or set of constraints.
- As a caller, I can release an allocation.
- As an operator, I can declare that a pool should contain four ready resources.
- As an operator, I can set min/max/warm capacity.
- As an operator, I can inspect desired state, observed state, allocations and pending operations.
- As a provider author, I can expose a new resource kind and its capabilities.
- As an operator, I can restart Fleetplane without losing operation state.
- As an operator, I can adopt or discover resources already present at the provider.
- As an operator, I can choose whether Fleetplane may delete adopted resources.

## 8. Success metrics

- Fresh control-plane restart can reconstruct managed state from storage + provider discovery.
- Repeated identical requests do not create duplicate infrastructure.
- Pool reconciliation converges after transient provider/API failures.
- Provider-specific VM lifecycle logic does not leak into the core.
- A second resource kind can be implemented without modifying the scheduler/reconciler kernel.
- A new compiled provider can be registered without editing central switch statements.
