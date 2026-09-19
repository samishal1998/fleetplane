---
title: "ADR-019: Parked machines (stop/resume warm tier)"
description: "Parked machines (stop/resume warm tier)."
sidebar:
  label: "019 · Parked machines (stop/resume warm tier)"
---

Status: accepted (implements docs/12; design adversarially verified, 29
findings reconciled — 5 blockers reshaped it as noted).

## Decisions

1. **Three new phases** — `parking`, `parked`, `starting` — via a
   NO-TRANSACTION table rebuild (0004): SQLite cannot alter CHECKs and
   `PRAGMA foreign_keys` is a no-op inside a transaction, so the rebuild
   runs outside one on the single-connection writer (the documented
   exception to ADR-010's DSN-only pragma rule). A populated-DB migration
   test guards it. `parked → orphaned` and `orphaned → parked` (re-observed
   stopped) are legal — a stopped machine deleted in the cloud console must
   not wedge (blocker). `parked_at` stamps every entry to parked and clears
   on return to ready.

2. **Stop/start are idempotent driver actions** (`ParkAware` capability,
   type-asserted; conformance subtests enforce stop-of-stopped and
   start-of-running succeed). They take a delete-shaped journal path:
   EffectMaybe → plain re-dispatch, verifying → journaled immediately,
   attempt-capped (8) so a permanently-retryable error reverts instead of
   wedging (blocker). Ops are `resource.stop` / `resource.start`.

3. **Failure dispositions split by physical state** (blocker): a failed
   stop reverts `parking → ready`; a failed start reverts `starting →
   parked` (re-stamping `parked_at` — a just-demanded machine never gets
   stage-2 deleted moments later); a start that SUCCEEDED at the provider
   but exhausted the readiness probe goes `starting → failed` — the
   machine is running at full price and unhealthy, so failed-cleanup
   deletes it rather than recording a price tier it is not in.

4. **Two-stage reclaim**: stage 1 at `idleAfter` parks on capable
   providers (`reclaim.park: auto|never`, `""` ≡ auto everywhere) with the
   same in-tx gates as cost deletes plus the capability gate inside
   `JournalStop` (a provider that cannot park never sees a stop action —
   the gate lives in the transaction, not the caller). Stage 2
   (`deleteAfter`, measured from `parked_at`) is **poolless-only**: pool
   fleet size is owned by replicas convergence, and pool-side deleteAfter
   would churn delete/create forever (blocker) — pool specs reject it.

5. **Scheduler ladder**: ready-reserve → **start a parked machine**
   (pre-bind + `parked→starting` CAS serializing claimers) → queue → create.
   A failed start re-pends the acquisition (never AcqFailed) and a
   per-resource start-failure backoff stops the broken-best-fit ping-pong
   (blocker); `Scheduler.Resume` re-pends provisioning acquisitions whose
   machine reverted to parked across a crash. Queue estimation covers the
   `starting` phase (p50 of observed starts → driver `StartEstimate` → 60s).

6. **Pools**: `minRunning` (explicit opt-in) keeps a hot floor; the park
   pass respects BOTH floors (`minReady` on the instant tier, `minRunning`
   on the powered tier); warm-up starts parked machines before creating;
   replica reduction consumes the parked tier FIRST via direct journaled
   deletes (blocker: surplus drain could only reach ready machines),
   which also keeps the hot count above `minRunning` during reductions.

7. **Discovery**: re-adopted/restored machines mint with the phase their
   observation shows (stopped → parked on capable providers) — a restored
   parked fleet must not become schedulable "ready" records.

8. **API**: `POST /v1/resources/{id}:park` (`resource.delete`) and
   `:start` (`resource.create`), idempotent status matrix (202 fresh /
   200 already-there / 409 unsupported-or-conflict); `status.parkedAt` in
   the envelope. Operator `:park` bypasses the delete-protected and
   queued-work gates (explicit intent); the reclaim sweep respects both.

9. **Metrics**: `fleetplane_resource_park_total`, `_start_total`,
   `_start_seconds` (journal→ready incl. probe — the queue-estimate
   boundary), plus `phase="parked"` in the existing resource gauge.

10. **Deferred with seams**: creating directly into a stopped state,
    cost-aware park-vs-delete arithmetic against residual disk/IP prices,
    out-of-band stop adoption (observation-only today), persisted start
    backoff.
