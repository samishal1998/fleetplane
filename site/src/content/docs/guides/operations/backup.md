---
title: "Backup and restore"
description: "Hot backups through the ops listener, scheduled backups, and the restore procedure."
---

Hot backup runs `VACUUM INTO` on a dedicated connection — safe under WAL,
and kernel writes never queue behind it. Two equivalent entry points:

```bash
# CLI (talks to the ops listener; --ops-addr defaults to $FLEETPLANE_OPS_ADDR,
# else http://127.0.0.1:9090). The path is on the SERVER host.
fleetplane admin backup --to /var/backups/fleetplane/fleetplane-$(date +%F-%H%M).db
```

```bash
# Raw HTTP
curl -X POST http://127.0.0.1:9090/admin/backup \
  -H 'Content-Type: application/json' \
  -d '{"to":"/var/backups/fleetplane/fleetplane-2026-08-15.db"}'
# {"to":"/var/backups/fleetplane/fleetplane-2026-08-15.db","status":"ok"}
```

The CLI's HTTP timeout is 10 minutes — `VACUUM INTO` scales with database
size. Schedule backups hourly via a systemd timer or cron, and **always take
one immediately before an upgrade** (migrations are forward-only,
[ADR-010](/fleetplane/developers/adr/adr-010-migrations/)).

```ini
# /etc/systemd/system/fleetplane-backup.service
[Unit]
Description=Fleetplane hourly backup

[Service]
Type=oneshot
User=fleetplane
ExecStart=/bin/sh -c '/usr/local/bin/fleetplane admin backup --to /var/backups/fleetplane/fleetplane-$(date +%%F-%%H%%M).db'
```

```ini
# /etc/systemd/system/fleetplane-backup.timer
[Unit]
Description=Hourly Fleetplane backup

[Timer]
OnCalendar=hourly
Persistent=true

[Install]
WantedBy=timers.target
```

**Restore** (full procedure and post-restore drift semantics in the
[backup-restore runbook](#restore-runbook)):

1. Stop the service.
2. Replace the database file at `storage.path`; **delete any stale
   `-wal`/`-shm` siblings**.
3. Start the service. Readiness stays false until migrations and journal
   recovery complete (mutations are gated meanwhile).
4. Reconcile: `fleetplane pools reconcile <pool>` per pool, or wait for the
   periodic pass.
5. Verify `fleetplane resources` against the provider console and watch
   `fleetplane_operations{state="uncertain"}`.

Discovery repairs restore drift automatically: labeled machines the backup
doesn't know are re-adopted as managed, records whose machines are gone
become `orphaned` and are tombstoned after the grace window, and
duplicate-create leftovers are reclaimed as ghosts (details below).

## Restore runbook

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
