---
title: "ADR-020: Docker as the credential-free end-to-end substrate"
description: "Docker as the credential-free end-to-end substrate."
sidebar:
  label: "020 · Docker as the credential-free end-to-end substrate"
---

Status: accepted.

## Context

Every kernel proof ran against the deterministic fake; the only real
provider tests were env-gated on cloud credentials nobody has in a fork or
on a laptop. A container is a VM for every property the kernel cares about:
it is created asynchronously by an external authority, carries labels,
answers list and inspect, can be stopped and resumed, and outlives a
control-plane crash.

## Decisions

1. **`providers/docker` is a first-class driver**, compiled into the default
   binary like the cloud drivers, not test-only scaffolding. Containers are
   `compute.machine`; `serverType` is `<cpu>x<memMiB>` mapped to cgroup
   limits and reported back as capacity; `image` is `name:<ref>` (pulled on
   first use). It implements `ParkAware` (stop/start).

2. **Stdlib HTTP over the Engine socket, no moby client.** The API surface
   needed is seven endpoints of plain JSON; the moby module tree is large and
   pulls OpenTelemetry. `net/http` with a unix-dialing transport keeps the
   driver at the same dependency weight as the kernel's boundary rules
   assume for `pkg/sdk`.

3. **The create-then-start window is closed by observation.** Docker
   create is two calls. A crash between them leaves a labeled `created`
   container; op-label dedup adopts it and `ObserveOperation` re-issues the
   idempotent start, so recovery never makes a second container. A
   container that exits before running fails the create op terminally with
   the exit code — a wrong `command` fails loudly instead of wedging.

4. **`HostConfig.Init` is always set** so PID 1 forwards SIGTERM and stop is
   immediate rather than the 10s timeout-then-SIGKILL; the default command
   is `sleep 2147483647` (portable across busybox and coreutils).

5. **Container names are `fp-<resourceID>`**: unique by construction, so
   the 409-on-duplicate-name path never needs an EffectMaybe conflict
   classification.

6. **Tests gate on reachability, with a CI override.** Locally a missing
   daemon skips; `FLEETPLANE_E2E_DOCKER=1` (set in CI) turns the skip into
   a failure so the E2E cannot silently vanish. Every test container carries
   `fleetplane.io/test=1` under a per-run owner and is swept at cleanup.

## Consequences

- `tests/e2e_docker_test.go` runs the unchanged kernel — service,
  scheduler, journal, reclaim, park/resume, crash recovery — against a real
  provider on every CI run and every developer machine with Docker.
- TCP/HTTP readiness probes reach container bridge IPs only on Linux hosts;
  on Docker Desktop (macOS/Windows) use classes without a probe.
- Deferred: readiness-probe E2E with a listening image; docker `paused`
  as a distinct tier (currently observed as stopped).
