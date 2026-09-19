---
title: "Docker"
description: "Containers on a Docker Engine as machines — a real, credential-free provider for laptops and CI."
---

This page covers the `docker` driver: settings, sizes and images, parking, identity labels and end-to-end testing. For how providers fit together, see the [providers overview](/fleetplane/guides/providers/overview/).

Docker containers stand in for VMs: a real, asynchronous, label-addressable
provider that runs on any developer machine or CI runner with no cloud
credentials ([ADR-020](/fleetplane/developers/adr/adr-020-docker-provider/)). Create+start is
provisioning, stop/start is parking ([design doc 12](https://github.com/samishal1998/fleetplane/blob/main/docs/12_PARKED_MACHINES.md)),
remove is delete. The driver speaks the Engine HTTP API over the socket with
the standard library only.

## Settings

| Setting | Type | Default | Behavior |
|---|---|---|---|
| `host` | string | `unix:///var/run/docker.sock` | Engine endpoint; `tcp://host:2375` also accepted. |
| `command` | []string | `["sleep","2147483647"]` | Container command. Must keep PID 1 alive: a container that exits fails its create operation with the exit code. |
| `network` | string | daemon default bridge | Docker network to attach containers to. |

No credentials: access is the socket's filesystem permission (the user
running `fleetplane serve` must be in the `docker` group or equivalent).

```yaml
providers:
  docker-local:
    driver: docker

classes:
  ci-container:
    kind: compute.machine
    provider: docker-local
    spec:
      serverType: 2x1024          # <cpu>x<memMiB> → cgroup limits + reported capacity
      image: name:alpine:3.20     # pulled on first use
    reclaim:
      idleAfter: 10m              # stop at idle (park)
      deleteAfter: 2h             # remove after 2h stopped
```

## Sizes and images

- `serverType` is `<cpu>x<memMiB>` (e.g. `2x1024`) or a bare `<cpu>`
  (no memory limit). Applied as `NanoCpus`/`Memory` and read back as
  capacity, so best-fit placement works as with clouds.
- `image` is `name:<reference>` (or a bare reference). Missing images are
  pulled once on first create; a failed pull is `invalid` (fail fast).
- `userData` is passed as the `FLEETPLANE_USER_DATA` environment variable.

## Parking

Implements `ParkAware`: stopped containers cost nothing but disk and resume
in well under a second (`startEstimate: 2s`). Stop is immediate because the
driver always sets `HostConfig.Init` so PID 1 forwards SIGTERM.

## Identity labels

Applied verbatim as Docker labels (`fleetplane.io/managed`, `/owner`, `/id`,
`/op`, plus `spec.labels`) and filtered server-side on discovery. Container
names are `fp-<resource id>` — unique by construction, readable in `docker ps`.

## Container status mapping

| Container status | Fleetplane observed phase |
|---|---|
| `created`, `restarting` | `pending` |
| `running` | `running` |
| `exited`, `paused` | `stopped` |
| `removing` | `deleting` |
| anything else | `unknown` |

A create whose container reaches `exited`/`dead` before `running` fails
terminally (wrong `command` or image); a crash between create and start
leaves a `created` container that recovery adopts and starts — never a
second container.

## End-to-end testing with Docker

`tests/e2e_docker_test.go` runs the unchanged kernel — service, scheduler,
journal, reclaim, park/resume, crash recovery — against real containers, and
`providers/docker` runs the full conformance catalog. Both skip when no
daemon is reachable; `FLEETPLANE_E2E_DOCKER=1` (set in CI) turns the skip
into a failure. Every test container carries `fleetplane.io/test=1` under a
per-run owner and is swept at cleanup.

```bash
go test ./providers/docker/ ./tests/ -run 'Docker' -v
```

Readiness probes (`spec.readiness.tcp|http`) reach container bridge IPs only
on Linux hosts; on Docker Desktop (macOS/Windows) use classes without a probe.
