---
title: "ADR-API-002: Completing the operation surface"
description: "Completing the operation surface."
sidebar:
  label: "API-002 · Completing the operation surface"
---

Status: Accepted (2026-09-19)

## Context

A capability audit walked the kernel for two shapes of gap: state the control
plane *reads* that nothing can *write*, and operations the storage layer
supports that no route reaches. It found both, in several places:

- `Pool.Paused` was read by the reconciler and returned by the API, but
  `UpsertPoolCmd` had no `Paused` field — nothing in the system could ever set
  it.
- `DeleteProtected` gated deletion, parking, exclusive scheduling and idle
  reclaim, and was surfaced as `metadata.protected` — but nothing could set it.
- Pools could not be removed at any layer: no `DELETE` route, no service
  method, and `PoolStore` had no `Delete` at all.
- The phase table permits `draining → ready` and the reconciler undrains
  automatically when a deficit reappears, but there was no manual verb, so an
  accidental drain was irreversible by hand.
- There was no `GET /v1/acquisitions`. The dashboard worked around it out of
  `localStorage`, with the honest-but-sad copy "The API has no acquisition
  list; shown below are ones created from this browser".
- `storage.ResourceFilter` supported `PoolID` and `Phases`, but the handler
  exposed only `kind`/`class`/`provider`. The dashboard's pool detail listed
  members by class instead and degraded to "Pool uses an inline spec; member
  listing by class is unavailable".
- `GET /v1/events` could not filter by resource, so a per-machine history meant
  filtering a capped page client-side — silently losing whatever did not fit.

## Decision

Close all of them, additively. On the wire nothing changes shape or status:
new routes, new query parameters, no existing response altered. ADR-API-001's
"all additive" holds for HTTP here too — but not for Go; see the last section.

### Pause is a verb, not a manifest field

`:pause` and `:resume` set `Pool.Paused`; `apply` never touches it. A bool has
no "unset": if `paused` were a manifest field, a manifest that simply omits it
would silently resume a pool an operator paused to investigate. The failure
mode is exactly backwards from what an operator expects of a declarative file,
so the flag stays imperative.

### Pools hard-delete; resources tombstone

ADR-017 pins deletion terminality for resources to the `deleted_at` tombstone.
Pools deliberately do the opposite and delete the row, because `pools.name` is
`UNIQUE` and operators reuse names — a tombstone would burn the name forever.

`foreign_keys` is ON, so the dead `pool_id` pointers on tombstoned resources
and terminal operations would otherwise block the delete for the lifetime of
the database: a pool that ever held a member could never be removed. The delete
clears those two, and only those two. Live members are deliberately *not*
cleared, so the FK refuses the `DELETE` and backstops the caller's gate rather
than quietly orphaning a running machine. Events keep the audit trail —
`events.pool_id` carries no FK.

### Protection is metadata, not desired state

`:protect` / `:unprotect` go through a dedicated
`ResourceStore.SetDeleteProtected` rather than `UpdateSpec`. Protection is an
operator's note about a resource, not a declared intent about the provider, and
routing it through `UpdateSpec` would bump `Generation` and churn
`observedGeneration` on every toggle — telling the reconciler a resource had
drifted when nothing about it had.

### Undrain reuses the reconciler's own transition

`:undrain` goes through the same `CASPhase(draining → ready)` the reconciler's
automatic undrain uses, so `ready_at` is restamped identically and the idle
sweeper measures from the return to service rather than from before the drain.
Racing the reconciler is safe by construction: it is a compare-and-swap, so one
side wins and the other reports a conflict.

### The acquisition listing defaults to live states

`GET /v1/acquisitions` without `?state` returns `pending`, `provisioning` and
`bound` only. Acquisitions are never garbage collected, so an unfiltered
listing is the control plane's entire history and grows without bound — the
default has to be the question operators actually ask ("who is holding what
right now"), with the terminal states available on request.

### `aborted` was examined and left alone

`OpAborted` is defined and terminal, and the scheduler and discovery both
handle it — but nothing anywhere produces it. That makes it dead state, not a
missing user operation: inventing an `:abort` verb would be designing a feature
to justify a constant, and a withdrawal-before-dispatch verb races the engine
for no operator benefit. It stays as it is, unreachable, until something needs
to produce it.

### Two breaking `pkg/apiclient` signatures

`ListResources` takes a `ResourceFilter` and `ListEvents` takes a `resource`
argument. Both break compilation for callers. Taken deliberately at v0.x, and
the alternative was worse: `ListResources` previously had no filter parameter
at all, so even the already-documented `?class=` was unreachable from the Go
client, and a parallel `ListResourcesFiltered` would leave that permanently as
the path of least resistance. A compile error naming the call site is the
cheapest possible migration.
