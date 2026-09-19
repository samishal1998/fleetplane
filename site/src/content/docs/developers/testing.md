---
title: "Testing"
description: "Unit and integration tests, the fault-injection catalogue, the kill -9 tier, chaos, and the Docker and Hetzner end-to-end suites."
---

## Test layout

- Unit tests live beside their packages (`internal/...`, `pkg/...`, `providers/...`).
- Cross-cutting integration tests live in [`tests/`](https://github.com/samishal1998/fleetplane/tree/main/tests) (API, pools, acquisition, ownership, readiness, chaos, fault injection).
- End-to-end tests against a real Hetzner project are opt-in: set `FLEETPLANE_E2E=1` and `HETZNER_TOKEN` (dedicated throwaway project only — see [ADR-015](/fleetplane/developers/adr/adr-015-e2e-safety/); all test resources are labeled and swept).
- Chaos tests are gated behind `FLEETPLANE_CHAOS=1` and print their seed for reproduction.

`make test` runs everything that needs no credentials: `go test -race -shuffle=on -count=1 ./...`. Property tests use `pgregory.net/rapid` ([ADR-016](/fleetplane/developers/adr/adr-016-property-testing/)).

## The fault-injection catalogue

`tests/fi_test.go` holds one named test per failure scenario from the implementation plan, each pinning the [crash-safety invariants](/fleetplane/developers/architecture/#crash-safety-invariants) it protects. The in-process crash tier uses the harness's `Crash`/`Restart` against the fake provider, whose cloud survives the crash, so duplicate detection is exact.

| ID | Test | Scenario |
|---|---|---|
| FI-1 | `TestFI_CreateAcceptedButResponseLost` | The provider accepted the create but the response was lost. The engine must resolve by the op label — never blind-retry into a duplicate (invariants 2, 7). |
| FI-2 | `TestFI_CrashAfterJournalBeforeProviderCall` | Crash after the journal write, before the provider call. The journaled state proves the provider was never called → exactly one safe re-dispatch. |
| FI-3 | `TestFI_CrashAfterProviderCreateBeforeCommit` | Crash after the provider accepted the create but before the ref committed (`in_flight`). Recovery adopts by op label (invariant 7). |
| FI-4 | `TestFI_DelayedListConsistency` | The lost-response create is invisible to Discover for several list passes; the engine must keep verifying inside the window — never re-dispatch early. |
| FI-4b | `TestFI_TrulyAbsent_RedispatchExactlyOnce` | The create truly never happened. Past the verify window the engine re-dispatches exactly once, with the same op ID. |
| FI-5 | `TestFI_RateLimitRespectsRetryAfter` | Rate limiting. Retries must respect pacing (no thundering herd) and still converge. |
| FI-6 | `TestFI_DuplicateClientRetry` | Duplicate client retry with the same idempotency key — one side effect, replayed result (invariant 2 at the service layer). |
| FI-7 | `TestFI_DeleteAlreadyDeleted` | Delete of an already-deleted resource is terminal success — the tombstone lands, no error loop ([ADR-017](/fleetplane/developers/adr/adr-017-operation-states/)). |

```bash
go test ./tests -run 'TestFI_' -v
```

### The `kill -9` tier

`tests/subprocess_test.go` (`TestSubprocess_Kill9DuringCreate_RecoversToReady`) builds the real binary, kills it with `SIGKILL` mid-create, and restarts it over the same database — proving WAL recovery and journal resume through an actual process boundary. It is skipped under `-short`.

## Chaos

```bash
# Nightly chaos suite: a seeded random schedule of acquisitions, releases,
# pool changes, fault injections, crashes and clock jumps — the system must
# converge with every invariant intact. The seed is printed; replay a
# failure exactly with CHAOS_SEED.
FLEETPLANE_CHAOS=1 go test ./tests -run TestChaos -v
CHAOS_SEED=1755264000000000000 FLEETPLANE_CHAOS=1 go test ./tests -run TestChaos -v
```

## Docker end-to-end tests

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

`tests/e2e_docker_test.go` includes `TestE2E_DockerLeaseParkResumeDelete` (acquire creates and binds; release plus idle parks the container; the next acquire resumes the same container with zero creates; stage 2 removes it) and `TestE2E_DockerCrashRecoveryNeverDuplicates`. Why Docker is a first-class driver rather than test scaffolding: [ADR-020](/fleetplane/developers/adr/adr-020-docker-provider/).

## Live Hetzner E2E

The Hetzner end-to-end test (`tests/e2e_hetzner_test.go`) runs the full
conformance suite against the real API. It is double-gated and skips unless
**both** are set:

```bash
FLEETPLANE_E2E=1 HETZNER_TOKEN=... go test ./tests/ -run TestE2E -v
```

Safety rules ([ADR-015](/fleetplane/developers/adr/adr-015-e2e-safety/)) — these are not optional:

- The token must belong to a **dedicated throwaway Hetzner project**. Never
  point E2E tests at a project containing anything you care about.
- Every E2E resource is labeled `fleetplane.io/test=1` and
  `fleetplane.io/test-run=<run id>`.
- Cleanup is layered: per-test `t.Cleanup`, an always-run CI sweeper step, and a
  nightly sweep. The sweeper (`scripts/e2esweep`) refuses to delete anything
  that does not carry `fleetplane.io/test=1` — conservative destruction applies
  to test tooling too.
- In CI, E2E runs only on main pushes, the nightly schedule, or manual dispatch
  — never on fork PRs.

## Provider conformance

Every driver runs the shared conformance catalogue from `pkg/sdk/conformance`; the subtests are the contract. See [writing a provider → the conformance suite](/fleetplane/developers/provider-sdk/#the-conformance-suite).
