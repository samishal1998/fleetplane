---
title: "Introduction"
description: "What Fleetplane is, what it gives you, and where to go next."
---

**An agentless control plane for VM fleets, shipped as a single static binary.**

Fleetplane creates, reuses, and reclaims cloud resources — virtual machines, volumes, and other provider-backed kinds — on demand. Run it on a very small always-on VM and let it create larger pre-imaged machines when work arrives (for example Hetzner Cloud CI runners), reuse existing capacity before creating more, and delete idle capacity by policy. No agent runs on the managed machines: Fleetplane talks only to provider APIs.

Everything mutating flows through a crash-safe **operation journal**: kill the process mid-create, restart it, and recovery converges to exactly the fleet you asked for — no duplicates, no leaks. `examples/demo.sh` proves this live with a `kill -9`.

## What Fleetplane gives you

- **Pools** — declare "keep N machines of this class"; the reconciler holds the fleet at that size with a bounded mutation budget per cycle.
- **Acquisitions and leases** — `fleetplane acquire` binds you to an existing machine when capacity is free, or creates one when it is not; leases carry TTLs and idle machines are reclaimed by policy.
- **Cost-aware leasing** — providers declare billing windows (e.g. per-started-hour); Fleetplane reuses already-paid capacity, queues acquisitions to pack work into paid windows, and times deletions to land before billing boundaries ([design doc 11](https://github.com/samishal1998/fleetplane/blob/main/docs/11_COST_AWARE_LEASING.md)).
- **Parked machines (the warm tier)** — where stopped machines bill at storage price (GCP, AWS), idle machines are parked instead of deleted and restarted in seconds instead of re-provisioned in minutes; the scheduler starts a parked machine before creating a new one ([design doc 12](https://github.com/samishal1998/fleetplane/blob/main/docs/12_PARKED_MACHINES.md)).
- **Crash-safe operations** — every provider call is journaled first; the operation engine is the only retry authority, and ambiguous outcomes are verified rather than guessed ([ADR-014](/fleetplane/developers/adr/adr-014-retries/), [ADR-017](/fleetplane/developers/adr/adr-017-operation-states/)).
- **Multi-provider** — the same kernel drives Hetzner Cloud, DigitalOcean, AWS, and GCP (plus a deterministic fake provider); orchestration code never imports a cloud SDK (enforced mechanically).
- **Dynamic classes** — machine templates managed via API/CLI/dashboard or config, validated against the kind registry at definition time.

## Key features

- Single static Go binary: server, CLI, and web dashboard in one `fleetplane` executable
- Providers: **Hetzner Cloud**, **DigitalOcean**, **AWS**, **GCP**, **Docker** (containers as machines — real end-to-end tests with no cloud account), and a **fake** provider for local development and tests
- Generic resource kinds: `compute.machine` and `storage.volume` (the kind registry is open to more)
- Declarative apply: `fleetplane apply -f` with multi-doc YAML `Pool` and `Resource` manifests, compiled onto the same imperative API
- Web dashboard embedded in the binary, served at `/ui/` — no extra deployment, works offline
- Prometheus metrics on a separate loopback ops listener (`/metrics`), plus pprof and hot backup
- Static token auth with a closed permission set; secrets only ever referenced as `secret://` — never stored in config
- `fleetplane cloud-init` renders a user-data file that bootstraps the control VM (install, config, secrets, systemd) on Ubuntu, Debian, Fedora/RHEL-family, Arch, or openSUSE
- SQLite storage (WAL) with online backup via `VACUUM INTO`
- Discovery sweep: detects orphans and ghosts in the provider account and converges them safely

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

## Core design rule

**The core must not know what a VM, database, GitHub runner, or Hetzner server is.**

The core knows resources, capabilities, desired/actual state, operations, pools, leases, constraints and reconciliation. Provider and higher-level modules attach domain meaning.

## Project status

Current release: **v0.8.0** — the operation surface is now complete: every state the kernel acts on can be set from the API, CLI, and dashboard (pool pause/resume/delete, resource undrain/protect/unprotect, acquisition listing, pool and phase filters). All phases of the [implementation plan](https://github.com/samishal1998/fleetplane/blob/main/docs/09_IMPLEMENTATION_PLAN.md) are complete — generic resource kernel, provider SDK, six drivers, pools and reconciliation, acquisition scheduling, cost-aware leasing, parked machines, CLI/API hardening, production hardening (metrics, backup, chaos tests), and the `storage.volume` kind as the genericity proof. The API version is `fleetplane.io/v1alpha1`; expect additive evolution.

## Where to go next

- New here? Read the [concepts](/fleetplane/guides/concepts/), then run the [quickstart](/fleetplane/guides/quickstart/) — no cloud account needed.
- Going to production? Start at [installation](/fleetplane/guides/installation/) and the [configuration reference](/fleetplane/guides/configuration/).
- Contributing? The [architecture overview](/fleetplane/developers/architecture/) explains the operation journal and the crash-safety invariants everything else rests on.
