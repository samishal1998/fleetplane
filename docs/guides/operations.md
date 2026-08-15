# Operations guide

How to run `fleetplane serve` in production: supervision, listeners, health
semantics, metrics and alerting, backup, the uncertain-operation runbook,
discovery behavior, shutdown, and upgrades.

Fleetplane v1 is a single-node control plane backed by SQLite
([design docs 06](../06_STORAGE_AND_HA.md)): run exactly one `fleetplane serve`
process per database file. Treat the host as privileged infrastructure — it
holds credentials that can create and destroy machines
([design docs 07](../07_SECURITY_AND_OPERATIONS.md)).

## Process supervision (systemd)

`fleetplane serve` is a single foreground process. It logs JSON to stderr,
shuts down gracefully on the first `SIGINT`/`SIGTERM`, and force-exits with
code `130` on a second signal — which maps cleanly onto systemd:

```ini
# /etc/systemd/system/fleetplane.service
[Unit]
Description=Fleetplane control plane
After=network-online.target
Wants=network-online.target

[Service]
User=fleetplane
Group=fleetplane
ExecStart=/usr/local/bin/fleetplane serve --config /etc/fleetplane/config.yaml
# Provider credentials for secret://env/... references (e.g. HETZNER_TOKEN=...)
EnvironmentFile=-/etc/fleetplane/secrets.env
Restart=on-failure
RestartSec=2

# First SIGTERM starts a graceful shutdown bounded by server.shutdownGrace
# (default 20s). Give systemd a little more than that before it escalates.
TimeoutStopSec=30

# Hardening (adjust ReadWritePaths to your storage.path and backup target)
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/fleetplane /var/backups/fleetplane

[Install]
WantedBy=multi-user.target
```

Notes:

- `--config` is required; the file is strict YAML — unknown keys and invalid
  values are boot errors, never silent (see the
  [configuration reference](configuration.md)).
- `--log-level` accepts `debug|info|warn|error` (default `info`).
- Credentials never live in the config file — use `secret://env/NAME` (reads
  the environment, e.g. from `EnvironmentFile=`) or
  `secret://file/<absolute path>`.
- If no `auth.tokens` are configured, the API runs **open** and boot logs
  `NO API TOKENS CONFIGURED — the API is open; use auth.tokens in production (07 §2)`.
  Configure tokens before exposing the listener (see
  [configuration → auth](configuration.md#auth)).

Logs are structured JSON, one object per line, tagged with the instance's
`owner_id`:

```bash
journalctl -u fleetplane -f -o cat | jq -r '[.time,.level,.msg] | @tsv'
```

The line to wait for after a (re)start:

```json
{"level":"INFO","msg":"fleetplane ready","addr":"[::]:8080","opsAddr":"127.0.0.1:9090","db":"/var/lib/fleetplane/fleetplane.db","providers":["hetzner-main"]}
```

## The two listeners

| Listener | Config key | Default | Serves |
|---|---|---|---|
| Main API | `server.addr` | `":8080"` | `/v1/*` (behind the mutation gate), `/health/live`, `/health/ready`, and the embedded [web dashboard](dashboard.md) at `/ui/` (`/` redirects there) |
| Ops | `server.opsAddr` | `"127.0.0.1:9090"` | `/health/*`, `GET /metrics`, `GET /debug/pprof/*`, `POST /admin/backup` |

> **Keep the ops listener loopback-only.** It has **no authentication**:
> `/metrics` leaks pool/provider names through labels, `pprof` exposes
> process internals, and `POST /admin/backup` writes files on the server
> host. Reach it via SSH tunnel or a trusted management network, never the
> public internet.

The main listener is the one to put behind your load balancer or reverse
proxy; every client command of the [CLI](cli.md) and the dashboard talk to it.
The only CLI command that talks to the ops listener is
`fleetplane admin backup`.

The dashboard is embedded in the binary — no extra deployment, works
offline. Log in by pasting an API token (stored in the browser's
localStorage); with no tokens configured the API is open and the dashboard
needs none. See the [dashboard guide](dashboard.md).

## Health and readiness

Both listeners serve the same two endpoints:

| Endpoint | Meaning |
|---|---|
| `GET /health/live` | Always `200` while the process runs. Use as the liveness/restart probe. |
| `GET /health/ready` | `200` only after **storage ping ∧ journal recovery ∧ acquisition resume** completed at boot, and while storage stays reachable. Use as the traffic/readiness probe. |

```bash
curl -s http://127.0.0.1:8080/health/ready
# {"status":"ok"}          — ready
# {"status":"unready"}                    (HTTP 503) — recovery not complete / shutting down
# {"status":"unready","reason":"storage"} (HTTP 503) — DB unreachable
```

Until readiness flips true (and again during shutdown drain), the **mutation
gate** rejects every non-GET/HEAD request under `/v1/*` with `503`:

```json
{"error":{"code":"unready","message":"recovery in progress or shutting down","retryable":true}}
```

plus a `Retry-After: 2` header. Reads always pass — you can inspect state
while recovery is running.

> **Provider health never gates readiness.** A degraded or unavailable cloud
> provider does not make the control plane unready
> ([design docs 07 §8](../07_SECURITY_AND_OPERATIONS.md)) — taking the API
> down would only remove your ability to see and fix the problem. Provider
> health is surfaced separately, in three places:
>
> - `fleetplane providers` / `GET /v1/providers` (state, consecutive
>   failures, last error),
> - the `fleetplane_provider_health_state` metric,
> - the dashboard's Overview and Providers views.

Provider health is a per-instance state machine
`unknown → healthy → degraded → unavailable`: checked every 30s, `degraded`
after 3 consecutive failures, `unavailable` after 10; one success snaps back
to `healthy`.

```bash
fleetplane providers
# INSTANCE      DRIVER   STATE    FAILURES  LAST ERROR
# hetzner-main  hetzner  healthy  0
```

If recovery fails at boot (storage unreachable, or journal recovery errors —
logged as `storage unavailable at boot` / `journal recovery failed; mutations
stay gated`), the process stays up and serves reads, but remains unready
until it is restarted after the cause is fixed.

## Prometheus metrics

`GET /metrics` on the **ops listener** exposes the fleet gauges plus the
standard Go runtime collector. Fleet gauges are computed from storage on each
scrape — there is no event-driven gauge drift.

```yaml
# prometheus.yml — scrape from the same host (ops listener is loopback)
scrape_configs:
  - job_name: fleetplane
    static_configs:
      - targets: ["127.0.0.1:9090"]
```

| Metric | Labels | Meaning |
|---|---|---|
| `fleetplane_resources` | `provider`, `kind`, `phase` | Resources by provider, kind and phase (tombstoned excluded) |
| `fleetplane_leases_active` | — | Active leases |
| `fleetplane_operations` | `state` | Non-terminal operations by state; `state="uncertain"` is **always** exported, even at 0 |
| `fleetplane_provider_health_state` | `provider`, `state` | Provider health (1 = current state) |
| `fleetplane_pools` | — | Configured pools |

Two label vocabularies you will alert on:

- Resource phases: `unknown`, `provisioning`, `ready`, `allocated`,
  `draining`, `deleting`, `failed`, `orphaned` (there is no `deleted` phase —
  deletion is a storage tombstone, [ADR-017](../adr/ADR-017-operation-states.md)).
- Non-terminal operation states: `journaled`, `in_flight`,
  `external_accepted`, `verifying`, `uncertain`.

Only `uncertain` is guaranteed to exist as a series; the other operation
states appear only while such operations exist, so write alert expressions
that tolerate absent series.

### Cost-aware leasing metrics

Nine series cover
[cost-aware leasing](concepts.md#cost-aware-leasing-billing-windows)
([design doc 11](../11_COST_AWARE_LEASING.md),
[ADR-018](../adr/ADR-018-cost-aware-leasing.md)). Unlike the fleet gauges
they are event-driven and process-lifetime — they reset on restart, so use
`rate()`/`increase()` over them:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `fleetplane_resource_reuse_total` | counter | `class` | Acquisitions bound to already-existing capacity instead of a new create |
| `fleetplane_scale_up_avoided_total` | counter | `class` | Queued acquisitions that bound to existing capacity instead of scaling up |
| `fleetplane_acquisition_queue_seconds` | histogram | — | Time queued acquisitions waited before binding |
| `fleetplane_resource_termination_seconds` | histogram | `provider` | Delete operation duration, journal to provider-confirmed terminal |
| `fleetplane_billing_boundary_overruns_total` | counter | `provider` | Deletes of billing-aware resources confirmed after the boundary they targeted |
| `fleetplane_billing_window_missed_total` | counter | `provider` | Reclaim-eligible resources whose termination window passed unused (they wait a full extra increment) |
| `fleetplane_resource_paid_seconds_total` | counter | `provider`, `kind` | Billed lifetime of terminated billing-aware resources |
| `fleetplane_resource_useful_seconds_total` | counter | `provider`, `kind` | Leased (busy) time of terminated billing-aware resources — merged lease spans, not lease-seconds |
| `fleetplane_resource_paid_idle_seconds_total` | counter | `provider`, `kind` | Paid-but-idle time of terminated billing-aware resources (paid − useful, clamped at 0) |

Is the optimization working? Four signals: `paid_idle_seconds` should trend
**down** relative to `paid_seconds`, `scale_up_avoided_total` should be
**greater than 0** on queue-enabled classes, `boundary_overruns_total`
should stay **around 0** (if it climbs, raise the kind's `terminationBuffer`
or enable `adaptive`), and `window_missed_total` should stay **near 0**
(persistent misses mean the buffer and sweep cadence leave the termination
window practically unhittable).

### What to alert on

```yaml
groups:
  - name: fleetplane
    rules:
      # The one page-worthy alert: an operation is frozen awaiting a human.
      - alert: FleetplaneUncertainOperations
        expr: fleetplane_operations{state="uncertain"} > 0
        for: 1m
        labels: {severity: page}
        annotations:
          summary: "Uncertain operations need manual :resolve"
          runbook: "docs/guides/operations.md#uncertain-operations"

      - alert: FleetplaneProviderUnavailable
        expr: fleetplane_provider_health_state{state="unavailable"} == 1
        for: 5m
        labels: {severity: page}

      - alert: FleetplaneProviderDegraded
        expr: fleetplane_provider_health_state{state="degraded"} == 1
        for: 15m
        labels: {severity: warn}

      # A machine vanished provider-side; discovery will tombstone after grace.
      - alert: FleetplaneOrphanedResources
        expr: sum(fleetplane_resources{phase="orphaned"}) > 0
        for: 10m
        labels: {severity: warn}

      - alert: FleetplaneFailedResources
        expr: sum(fleetplane_resources{phase="failed"}) > 0
        for: 10m
        labels: {severity: warn}

      - alert: FleetplaneUnready
        expr: up{job="fleetplane"} == 0
        for: 2m
        labels: {severity: page}
```

`fleetplane_operations{state="uncertain"} > 0` is the alert this system is
designed around: `uncertain` means the engine has exhausted automatic
verification and **wants an operator** — nothing will move that operation
except a manual `:resolve` (next section).

## Backup

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
[ADR-010](../adr/ADR-010-migrations.md)).

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
[backup-restore runbook](../runbooks/backup-restore.md)):

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

## Uncertain operations

`uncertain` means Fleetplane could not prove whether a provider mutation
happened and **refuses to guess**. The operation went
`journaled → in_flight`, the outcome of the provider call is unknown (crash,
timeout, ambiguous transport error), and post-crash verification (`verifying`)
exhausted its window (`engine.verifyWindow`, default `120s`) without proof
either way. The operation is frozen; no retry authority other than a human
exists at this point ([ADR-014](../adr/ADR-014-retries.md),
[ADR-017](../adr/ADR-017-operation-states.md)).

### Inspect

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

### Resolve

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

## Discovery, orphans, and ghosts in practice

The discovery sweep (every `discovery.interval`, default `30s`) lists each
provider's resources and reconciles observations against local records —
[ADR-017](../adr/ADR-017-operation-states.md) defines the vocabulary. The
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
[configuration → discovery](configuration.md#discovery).

## Graceful shutdown

On the first `SIGINT`/`SIGTERM` (`systemctl stop`):

1. Readiness flips to false — load balancers stop sending traffic, and the
   mutation gate 503s new mutations.
2. In-flight HTTP requests drain, bounded by `server.shutdownGrace`
   (default `20s`).
3. Provider connections and the store close.

A second signal forces an immediate exit with code `130`.

Provider-side operations are **never assumed stopped** when the process
exits: their state is in the journal, and the next boot's recovery resumes
them (that is what readiness waits for). A crash mid-operation therefore
cannot lose or double-apply a mutation — at worst it produces an `uncertain`
operation for you to resolve.

## Upgrades

Schema migrations are embedded in the binary and run automatically at boot,
before the listeners report ready (goose, per-migration transactions —
[ADR-010](../adr/ADR-010-migrations.md)). The procedure:

```bash
# 1. Back up first — migrations are forward-only.
fleetplane admin backup --to /var/backups/fleetplane/pre-upgrade-$(date +%F).db

# 2. Replace the binary and restart.
install -m 0755 fleetplane-new /usr/local/bin/fleetplane
systemctl restart fleetplane

# 3. Verify.
fleetplane version
curl -s http://127.0.0.1:8080/health/ready
journalctl -u fleetplane -o cat | grep '"msg":"fleetplane ready"' | tail -1
```

Then watch `fleetplane_operations{state="uncertain"}` and
`fleetplane providers` for a few minutes.

**Downgrade guard.** The server refuses to start against a database whose
schema is newer than the binary:

```text
database schema version 12 is newer than this binary's max 11: refusing to start (downgrade guard, ADR-010)
```

There is no automatic downgrade path. To roll back a binary after its
migrations have run, restore the pre-upgrade backup (the one step 1 exists
for) and follow the [restore procedure](../runbooks/backup-restore.md) —
discovery re-adopts anything created between backup and rollback.

## Testing gates for operators

The correctness properties this guide relies on are continuously tested;
both heavyweight suites are environment-gated so `make test` stays fast and
safe. Run them from a source checkout:

```bash
# Nightly chaos suite: a seeded random schedule of acquisitions, releases,
# pool changes, fault injections, crashes and clock jumps — the system must
# converge with every invariant intact. The seed is printed; replay a
# failure exactly with CHAOS_SEED.
FLEETPLANE_CHAOS=1 go test ./tests -run TestChaos -v
CHAOS_SEED=1755264000000000000 FLEETPLANE_CHAOS=1 go test ./tests -run TestChaos -v
```

```bash
# Live Hetzner E2E conformance — costs real money and creates real servers.
# ONLY against a dedicated throwaway Hetzner project (ADR-015): the tests
# and the sweeper refuse to touch resources not labeled fleetplane.io/test.
FLEETPLANE_E2E=1 HETZNER_TOKEN=... go test ./tests -run TestE2E_HetznerConformance -v
```

`make gate` is the full local green gate (build, vet, formatting, tidy,
race-enabled tests, boundary checks, lint —
[ADR-009](../adr/ADR-009-boundaries.md)); run it before deploying a build
you compiled yourself.

## See also

- [Configuration reference](configuration.md) — every key, default, and
  validation rule
- [CLI reference](cli.md) — every command, flag, and exit code
- [Dashboard guide](dashboard.md) — the embedded web UI
- [Backup and restore runbook](../runbooks/backup-restore.md)
- [Design docs 07: Security and operations](../07_SECURITY_AND_OPERATIONS.md)
  and [06: Storage and HA](../06_STORAGE_AND_HA.md)
- [ADR-017: Operation states, tombstone deletion, ghost vs orphan](../adr/ADR-017-operation-states.md)
- [ADR-014: The operation engine is the only retry authority](../adr/ADR-014-retries.md)
