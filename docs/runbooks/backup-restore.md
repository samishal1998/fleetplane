# Runbook: Backup and Restore

## Backup

Hot backup (safe under WAL; runs `VACUUM INTO` on a dedicated connection, so
kernel writes never queue behind it):

```sh
fleetplane admin backup --to /backups/fleetplane-$(date +%F-%H%M).db
```

The command talks to the **ops listener** (`server.opsAddr`, loopback by
default) — run it on the server host or over a tunnel. Schedule hourly via
cron/systemd-timer, and always take one before an upgrade (the server also
refuses to start against a database newer than the binary — ADR-010).

The ops listener also serves `/metrics` and `pprof`; never expose it on an
untrusted network (metric labels leak pool/provider names).

## Restore

1. Stop the service.
2. Replace the database file; **delete any stale `-wal`/`-shm` siblings**.
3. Start the service. Readiness stays false until migrations and journal
   recovery complete (mutations are gated meanwhile).
4. Reconcile: `fleetplane pools reconcile <pool>` per pool (or wait for the
   periodic pass).

What discovery does with drift after a restore (ADR-017):

- Machines the old backup doesn't know **but carrying our labels** are
  **re-adopted as managed** — resources never become undeletable because
  local state was lost (docs/06 §8). Their class label restores the idle
  reclaim policy.
- Records whose machines no longer exist become `orphaned`, and are
  tombstoned only after the grace window, with zero active leases, while
  the provider is healthy.
- Machines from a duplicate-create race (ghosts) are deleted through the
  normal journal (`discovery.ghostPolicy: surface` to disable).

Verify: `fleetplane resources` against the provider console; watch
`fleetplane_operations{state="uncertain"}` — anything stuck there needs
`fleetplane operations` + `POST /v1/operations/{id}:resolve`.

## Uncertain operations

`uncertain` means Fleetplane could not prove whether a provider mutation
happened and refuses to guess (invariant 7). Resolve manually:

```sh
fleetplane operations                       # find the op
# after checking the provider console:
curl -X POST "$FLEETPLANE_ADDR/v1/operations/<op_id>:resolve" \
  -H "Authorization: Bearer $FLEETPLANE_TOKEN" \
  -d '{"action":"retry-verification"}'      # or "mark-failed"
```

`retry-verification` re-runs label-based discovery with a fresh window;
`mark-failed` attests that the mutation did not happen (the resource goes to
`failed` and is replaced by pool policy).
