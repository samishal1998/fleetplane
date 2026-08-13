# Fleetplane

Fleetplane is an **agentless, provider-driven control plane for infrastructure resources**.

Its first-class use case is virtual-machine fleets: for example, running the control plane on a very small Hetzner instance while creating and destroying larger pre-imaged machines on demand for CI workloads. The architecture is intentionally generic enough that providers can expose other resources—databases, volumes, load balancers, bare-metal servers, or future resource types—without changing the core orchestration model.

## Product position

Fleetplane is not Kubernetes for containers, Terraform for provisioning, or a GitHub Actions-specific autoscaler.

It is a small control-plane kernel that:

1. accepts imperative resource requests;
2. optionally stores desired state;
3. discovers actual state through providers;
4. plans the delta;
5. executes mutations through providers;
6. reconciles drift;
7. exposes reusable primitives for pools, leases, capacity, placement, lifecycle, and scaling.

The initial implementation is **Go**, with **compile-time plugins/modules**. Providers are compiled into the final binary, similar in spirit to Caddy/CoreDNS custom builds. Runtime RPC/WASM extensions may be added later, but are explicitly not required for v1.

## Documents

- `01_PRD.md` — product requirements and scope.
- `02_ARCHITECTURE.md` — system architecture and core abstractions.
- `03_PROVIDER_SDK.md` — provider/plugin contract.
- `04_API_AND_RESOURCE_MODEL.md` — API, generic resource model, pools, leases, intents.
- `05_RECONCILIATION_AND_SCHEDULING.md` — reconciliation, placement, capacity and autoscaling.
- `06_STORAGE_AND_HA.md` — SQLite-first persistence, storage abstraction, HA path.
- `07_SECURITY_AND_OPERATIONS.md` — auth, secrets, auditability, failure handling and observability.
- `08_HETZNER_RUNNER_USE_CASE.md` — first concrete implementation.
- `09_IMPLEMENTATION_PLAN.md` — milestones, repository layout and acceptance criteria.
- `10_FUTURE_DSL.md` — declarative configuration and future DSL direction.

## Core design rule

**The core must not know what a VM, database, GitHub runner, or Hetzner server is.**

The core knows resources, capabilities, desired/actual state, operations, pools, leases, constraints and reconciliation. Provider and higher-level modules attach domain meaning.
