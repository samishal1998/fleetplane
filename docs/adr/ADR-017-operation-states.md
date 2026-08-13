# ADR-017: Operation states, tombstone deletion, ghost vs orphan

Status: Accepted (2026-08-13)

## Operation journal states (single vocabulary)

`journaled → in_flight → external_accepted → succeeded | failed`, plus `verifying` (post-crash/ambiguous-transport), `uncertain` (frozen; manual `:resolve` only), `aborted` (from `journaled` only). All transitions are CAS on the from-state. The crash partition: `journaled` = provider provably never called (safe re-dispatch); `in_flight` = unknown (must verify before any re-mutation) — invariant 7 as schema.

## Deletion terminality

There is **no** `deleted` phase (doc 05 §7 lists eight phases; the CHECK constraint pins them). Deletion terminality is the storage tombstone: `deleted_at` set, row leaves all partial indexes.

## Ghost vs orphan

- `orphaned` (phase): a local record whose provider resource vanished. Exit: tombstone, only after a second direct-Get confirmation, gated on zero active leases and a healthy provider instance (an outage must not reclaim a leased VM).
- **Ghost** (disposition, not a phase): a provider resource carrying our managed/owner/op labels but bound to no live record (duplicate-create race, restore leftovers). Disposition: a normal journaled, policy-gated provider delete (`ghostPolicy: delete|surface`, default `delete`, always evented) — never a blind inline delete, never a leak.
- Discovery re-adopts unmatched resources carrying **our** labels as `managed` (doc 06 §8: never undeletable because local state was lost); `observed` is reserved for genuinely foreign/unlabeled resources.
