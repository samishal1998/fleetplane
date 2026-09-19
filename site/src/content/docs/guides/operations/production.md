---
title: "Running in production"
description: "Process supervision, the two listeners, health and readiness, graceful shutdown, upgrades and troubleshooting."
---

Fleetplane v1 is a single-node control plane backed by SQLite
([design docs 06](https://github.com/samishal1998/fleetplane/blob/main/docs/06_STORAGE_AND_HA.md)): run exactly one `fleetplane serve`
process per database file. Treat the host as privileged infrastructure — it
holds credentials that can create and destroy machines
([design docs 07](https://github.com/samishal1998/fleetplane/blob/main/docs/07_SECURITY_AND_OPERATIONS.md)).

This page covers keeping the process healthy. The rest of day-2 operations has its own pages: [metrics and alerting](/fleetplane/guides/operations/metrics/), [backup and restore](/fleetplane/guides/operations/backup/), [uncertain operations and discovery](/fleetplane/guides/operations/uncertain-operations/), and [tokens and permissions](/fleetplane/guides/operations/tokens/).

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
  [configuration reference](/fleetplane/guides/configuration/)).
- `--log-level` accepts `debug|info|warn|error` (default `info`).
- Credentials never live in the config file — use `secret://env/NAME` (reads
  the environment, e.g. from `EnvironmentFile=`) or
  `secret://file/<absolute path>`.
- If no `auth.tokens` are configured, the API runs **open** and boot logs
  `NO API TOKENS CONFIGURED — the API is open; use auth.tokens in production (07 §2)`.
  Configure tokens before exposing the listener (see
  [configuration → auth](/fleetplane/guides/configuration/#auth)).

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
| Main API | `server.addr` | `":8080"` | `/v1/*` (behind the mutation gate), `/health/live`, `/health/ready`, and the embedded [web dashboard](/fleetplane/guides/dashboard/) at `/ui/` (`/` redirects there) |
| Ops | `server.opsAddr` | `"127.0.0.1:9090"` | `/health/*`, `GET /metrics`, `GET /debug/pprof/*`, `POST /admin/backup` |

:::caution[Keep the ops listener loopback-only]
It has **no authentication**:
`/metrics` leaks pool/provider names through labels, `pprof` exposes
process internals, and `POST /admin/backup` writes files on the server
host. Reach it via SSH tunnel or a trusted management network, never the
public internet.
:::

The main listener is the one to put behind your load balancer or reverse
proxy; every client command of the [CLI](/fleetplane/reference/cli/) and the dashboard talk to it.
The only CLI command that talks to the ops listener is
`fleetplane admin backup`.

The dashboard is embedded in the binary — no extra deployment, works
offline. Log in by pasting an API token (stored in the browser's
localStorage); with no tokens configured the API is open and the dashboard
needs none. See the [dashboard guide](/fleetplane/guides/dashboard/).

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

:::note[Provider health never gates readiness]
A degraded or unavailable cloud
provider does not make the control plane unready
([design docs 07 §8](https://github.com/samishal1998/fleetplane/blob/main/docs/07_SECURITY_AND_OPERATIONS.md)) — taking the API
down would only remove your ability to see and fix the problem. Provider
health is surfaced separately, in three places:

- `fleetplane providers` / `GET /v1/providers` (state, consecutive
  failures, last error),
- the `fleetplane_provider_health_state` metric,
- the dashboard's Overview and Providers views.
:::

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
[ADR-010](/fleetplane/developers/adr/adr-010-migrations/)). The procedure:

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
for) and follow the [restore procedure](/fleetplane/guides/operations/backup/) —
discovery re-adopts anything created between backup and rollback.

## Troubleshooting

Boot fails fast on configuration problems. Common errors:

| Symptom | Cause | Fix |
|---|---|---|
| `config: ... unknown field` | A key the binary does not implement (often a typo) — decoding is strict | Fix the key name; compare with [`examples/config.yaml`](https://github.com/samishal1998/fleetplane/blob/main/examples/config.yaml) |
| `config: storage.path is required` | Missing `storage.path` | Set the SQLite file path |
| `config: invalid duration "..."` | Duration not a Go duration string | Use forms like `20s`, `5m`, `1h30m` |
| `config: providers.<name>.driver is required` | Provider block without `driver` | Set `driver: hetzner`, `digitalocean`, `aws`, `gcp`, or `fake` |
| `config: classes.<name> needs kind and provider` | Incomplete class | Add `kind` and `provider` |
| `config: classes.<name> references unknown provider "..."` | Class points at an unconfigured provider | Match the class `provider` to a `providers` entry |
| `config: auth.tokens[N] needs id and sha256` | Incomplete token entry | Paste the full snippet from `fleetplane token new` |
| `config: auth.tokens[N].sha256 must be 64 hex chars` | Truncated or plaintext value in `sha256` | Store `hex(sha256(secret))`, not the secret |
| `config: auth.tokens: duplicate id "..."` | Two tokens share an `id` | Generate a fresh token |
| `auth.tokens[N]: unknown permission "..."` | Permission name outside the closed set | Use the names listed in [tokens and permissions](/fleetplane/guides/operations/tokens/) |
| `hetzner: token is required (secret:// reference)` (same for `digitalocean:`) | Missing/empty provider token setting | Set `settings.token` to a `secret://` reference |
| `aws: region is required` / `gcp: project is required` / `gcp: zone is required` | Missing required driver setting | Set `settings.region` (aws) or `settings.project` + `settings.zone` (gcp) |
| `aws: accessKeyId and secretAccessKey are all-or-nothing` | Only one of the static key pair set | Set both, or omit both to use the ambient credential chain |
| `gcp: credentialsJson must be a secret:// reference (07 §4)` | Literal value in `credentialsJson` | Use `secret://file/...` (or omit it to use Application Default Credentials) |
| `environment variable "NAME" is not set` | `secret://env/NAME` points at an unset variable | Export it in the service environment (systemd `EnvironmentFile`) |
| `listen <addr>: ... address already in use` | Another process holds `server.addr` or `server.opsAddr` | Free the port or change the address |
| `NO API TOKENS CONFIGURED` warning in logs | `auth.tokens` is empty — the API is open | Add tokens (see [tokens and permissions](/fleetplane/guides/operations/tokens/)) before exposing the listener |
| Mutating requests return `503` code `unready` | Journal recovery still running, or shutdown drain | Wait and retry (the response carries `Retry-After`); check `/health/ready` |
| `fleetplane_operations{state="uncertain"} > 0` | An operation exhausted verification and is frozen | Inspect with `fleetplane operations`, then resolve via `POST /v1/operations/{id}:resolve` or the dashboard — see the [operations guide](/fleetplane/guides/operations/uncertain-operations/) |

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
[ADR-009](/fleetplane/developers/adr/adr-009-boundaries/)); run it before deploying a build
you compiled yourself.

## See also

- [Configuration reference](/fleetplane/guides/configuration/) — every key, default, and
  validation rule
- [CLI reference](/fleetplane/reference/cli/) — every command, flag, and exit code
- [Dashboard guide](/fleetplane/guides/dashboard/) — the embedded web UI
- [Backup and restore runbook](/fleetplane/guides/operations/backup/)
- [Design docs 07: Security and operations](https://github.com/samishal1998/fleetplane/blob/main/docs/07_SECURITY_AND_OPERATIONS.md)
  and [06: Storage and HA](https://github.com/samishal1998/fleetplane/blob/main/docs/06_STORAGE_AND_HA.md)
- [ADR-017: Operation states, tombstone deletion, ghost vs orphan](/fleetplane/developers/adr/adr-017-operation-states/)
- [ADR-014: The operation engine is the only retry authority](/fleetplane/developers/adr/adr-014-retries/)
