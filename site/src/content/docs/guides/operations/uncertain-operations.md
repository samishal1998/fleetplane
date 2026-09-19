---
title: "Uncertain operations and discovery"
description: "What an uncertain operation means, how to inspect and :resolve it, and how discovery handles orphans and ghosts."
---

`uncertain` means Fleetplane could not prove whether a provider mutation
happened and **refuses to guess**. The operation went
`journaled → in_flight`, the outcome of the provider call is unknown (crash,
timeout, ambiguous transport error), and post-crash verification (`verifying`)
exhausted its window (`engine.verifyWindow`, default `120s`) without proof
either way. The operation is frozen; no retry authority other than a human
exists at this point ([ADR-014](/fleetplane/developers/adr/adr-014-retries/),
[ADR-017](/fleetplane/developers/adr/adr-017-operation-states/)).

## Inspect

```bash
fleetplane operations                 # list open operations
# ID          KIND    STATE      RESOURCE     ATTEMPT  ERROR
# op_01J...   create  uncertain  res_01J...   3        retryable

fleetplane operations op_01J...       # full JSON for one operation
fleetplane events --since 1h          # audit trail around it
```

Then check the provider console (Hetzner/DigitalOcean) for a machine
carrying the operation's label — Fleetplane tags every create with
`fleetplane.io/op` (encoded as an `fp-op:` tag on DigitalOcean), so the
machine, if it exists, is findable by operation ID.

## Resolve

`POST /v1/operations/{id}:resolve` (permission `provider.admin`) with one of
two actions; the API answers `202`. The dashboard's Operations view offers
the same two actions.

```bash
curl -X POST "$FLEETPLANE_ADDR/v1/operations/op_01J...:resolve" \
  -H "Authorization: Bearer $FLEETPLANE_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"action":"retry-verification"}'
```

| Action | Use when | Effect |
|---|---|---|
| `retry-verification` | You are unsure, or you fixed the underlying issue (provider outage over, credentials rotated) | Operation moves back to `verifying` with a fresh, generous deadline (48h); the engine re-runs label-based verification against the provider and finishes the operation normally |
| `mark-failed` | You checked the provider console and **attest the mutation did not happen** | Operation moves to `failed`; the resource's phase is CAS'd `provisioning → failed` (`deleting → failed` for delete operations); pool policy replaces it |

Prefer `retry-verification` — it is safe to repeat and lets the machine
decide. `mark-failed` is an attestation, and it is recorded as one: the audit
event carries `"operator attested the provider-side outcome"`.

If you `mark-failed` a create whose machine actually does exist, you have not
leaked it: the discovery sweep will find a labeled machine whose create
operation is `failed`, classify it as a **ghost**, and reclaim it through a
normal journaled delete (next section).

Both actions append an `operation.resolve` audit event with the outcome, so
`fleetplane events` shows who resolved what, and how.

## The :resolve call in full

When a provider call's outcome cannot be determined and the verification window is exhausted, the operation freezes in state `uncertain` and waits for an operator (the `fleetplane_operations{state="uncertain"}` metric counts these). Find it, decide what actually happened at the provider, then resolve:

```bash
curl -sS -H "Authorization: Bearer $TOKEN" "$BASE/v1/operations"
```

```bash
curl -sS -X POST "$BASE/v1/operations/op_01J5X0D4N7WCFT2QKYHB8VZRGM:resolve" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{ "action": "retry-verification" }'
```

```json
{ "id": "op_01J5X0D4N7WCFT2QKYHB8VZRGM", "action": "retry-verification" }
```

Actions (both return 202; requires `provider.admin`):

- `retry-verification` — thaw the operation back into `verifying` with a fresh verification window; use when the provider was temporarily unreachable.
- `mark-failed` — attest that the provider-side work did not happen; the operation becomes `failed` and the affected resource moves to phase `failed`.

Any other action string returns 400 `invalid`.

## Discovery, orphans, and ghosts in practice

The discovery sweep (every `discovery.interval`, default `30s`) lists each
provider's resources and reconciles observations against local records —
[ADR-017](/fleetplane/developers/adr/adr-017-operation-states/) defines the vocabulary. The
sweep only trusts providers whose health is `healthy` or `unknown`: against
a degraded provider it suspends orphan confirmation and logs
`provider unhealthy; orphan confirmation suspended` — absence during an
outage must never reclaim a leased machine.

Everything discovery decides is written to the audit log (`fleetplane
events`, dashboard Events view):

| Situation | What happens | Event type | Log line |
|---|---|---|---|
| Local record's machine vanished provider-side | Confirmed by a direct Get (a LIST miss is never proof), phase → `orphaned` | `resource.orphaned` | `resource orphaned (provider lost it); tombstone after grace` (warn) |
| Orphan stays gone past `discovery.orphanGrace` (default `60s`) | Tombstoned — only with **zero active leases** and a healthy provider | `resource.orphan_tombstoned` | — |
| Machine with Fleetplane labels, its create op terminally failed or bound elsewhere (duplicate-create race, post-restore leftover) | **Ghost**: with `discovery.ghostPolicy: delete` (default), reclaimed via a normal journaled delete — never a blind inline call | `resource.ghost` | `ghost reclaimed via journaled delete` |
| Same, with `discovery.ghostPolicy: surface` | Recorded and left alone for manual deletion | `resource.ghost` | `ghost surfaced (policy=surface); delete manually` (warn) |
| Machine with Fleetplane labels and no live record or journal evidence (restored/older database) | **Re-adopted as managed** — resources never become undeletable because local state was lost; the class label restores its reclaim policy | `resource.readopted` | `re-adopted labeled provider resource as managed (06 §8)` |
| Unlabeled machine, `discovery.adoptUnlabeled: observed` | Recorded read-only as `observed`; never mutated | `resource.observed` | — |
| Orphan whose machine reappears running | Phase flips back `orphaned → ready` | — | — |

Choose `ghostPolicy: surface` if you want a human in the loop before any
discovery-initiated deletion; the default `delete` is safe because ghost
deletes go through the same journal (and the same uncertainty handling) as
every other provider mutation. Config keys are documented in
[configuration → discovery](/fleetplane/guides/configuration/#discovery).
