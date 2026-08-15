# CLI reference

Fleetplane ships as a single binary. `fleetplane serve` runs the control plane; every
other command is a pure HTTP client of the public API — the CLI never touches the
database or kernel packages directly ([ADR-006](../adr/ADR-006-cli.md)). Anything the
CLI can do, plain HTTP (or [`pkg/apiclient`](https://github.com/samishal1998/fleetplane/tree/main/pkg/apiclient))
can do too.

The same server also serves a web dashboard at `http://<server.addr>/ui/` (`/` redirects
there). It is embedded in the binary — no extra deployment. See the
[dashboard guide](dashboard.md).

## Global flags

These persistent flags apply to every client command:

| Flag | Default | Description |
|---|---|---|
| `--addr` | `$FLEETPLANE_ADDR`, else `http://127.0.0.1:8080` | Fleetplane API address |
| `--token` | `$FLEETPLANE_TOKEN` | API token (`flp_<id>.<secret>`, see [ADR-008](../adr/ADR-008-tokens.md)) |
| `-o`, `--output` | `table` | Output format: `table` or `json` |

`-o json` affects the list-style commands (`resources`, `pools`, `operations`, `events`,
`providers`) and `acquire`. The single-object commands (`resources get`, `pools get`,
`operations ID`) always print JSON.

### Environment variables

| Variable | Used by | Meaning |
|---|---|---|
| `FLEETPLANE_ADDR` | all client commands | Default for `--addr` |
| `FLEETPLANE_TOKEN` | all client commands | Default for `--token` |
| `FLEETPLANE_OPS_ADDR` | `admin backup` | Default for `--ops-addr` |

An explicit flag always wins over its environment variable.

## Exit codes

From [ADR-006](../adr/ADR-006-cli.md):

| Code | Meaning |
|---|---|
| 0 | OK |
| 1 | Runtime error (network failure, bad manifest, …) |
| 2 | Usage error (unknown flag, missing argument) |
| 3 | Not found (HTTP 404) |
| 4 | Auth failure (HTTP 401/403) |
| 5 | Conflict / idempotency error (HTTP 409) |
| 6 | `watch` failed or timed out |
| 7 | Server error (HTTP 5xx) |
| 130 | `serve` forced exit on second signal |

Errors are printed to stderr as `fleetplane: <status> <code>: <message>`, using the
API's uniform error body:

```bash
fleetplane resources get res_does_not_exist
# fleetplane: 404 not_found: resource not found
# (exit code 3)
```

---

## fleetplane version

Print the binary version and the Go toolchain it was built with. The version is
injected at build time via `-ldflags "-X main.version=..."`; unreleased builds print
`dev`.

```bash
fleetplane version
```

```text
fleetplane dev (go1.26.5)
```

## fleetplane serve

Run the control plane in-process. This is the only command that does not talk HTTP to a
server — it *is* the server.

```bash
fleetplane serve --config /etc/fleetplane/config.yaml --log-level info
```

| Flag | Default | Description |
|---|---|---|
| `--config` | (required) | Path to the YAML config file |
| `--log-level` | `info` | `debug`, `info`, `warn`, or `error` |

Behavior:

- Logs are structured JSON on stderr.
- The first `SIGINT`/`SIGTERM` starts a graceful shutdown (drain HTTP within
  `server.shutdownGrace`, then close providers and storage). A second signal forces an
  immediate exit with code 130.
- Two listeners: the main API on `server.addr` (default `:8080`) and an ops listener on
  `server.opsAddr` (default `127.0.0.1:9090`) for `/metrics`, pprof, and
  `/admin/backup`.
- The web [dashboard](dashboard.md) is served on the main listener at `/ui/`.

See the [configuration guide](configuration.md) for the full config file reference.

## fleetplane resources

List resources, or manage one with the subcommands.

```bash
fleetplane resources
```

```text
ID                              KIND             PROVIDER      PHASE      EXTERNAL  NAME
res_01J8FYK2N9V1X4T7Q0C3E6H9SD  compute.machine  hetzner-main  ready      63201175  ci-large-1
res_01J8FYK7X2T4V9N1Q5C8E3H6SD  compute.machine  hetzner-main  allocated  63201312  ci-large-2
```

### fleetplane resources get

```bash
fleetplane resources get res_01J8FYK2N9V1X4T7Q0C3E6H9SD
```

Prints the full resource envelope as JSON:

```json
{
  "apiVersion": "fleetplane.io/v1alpha1",
  "kind": "Resource",
  "metadata": {
    "id": "res_01J8FYK2N9V1X4T7Q0C3E6H9SD",
    "name": "ci-large-1",
    "createdAt": "2026-08-15T09:12:44Z",
    "updatedAt": "2026-08-15T09:14:02Z"
  },
  "spec": {
    "kind": "compute.machine",
    "provider": "hetzner-main",
    "class": "ci-large"
  },
  "status": {
    "phase": "ready",
    "externalId": "63201175",
    "capacity": {"cpu": 4, "memoryMiB": 8192}
  }
}
```

### fleetplane resources create

Create a resource from a JSON manifest (`-f` is required).

| Flag | Default | Description |
|---|---|---|
| `-f`, `--file` | (required) | JSON manifest file |
| `--idempotency-key` | — | Idempotency key (safe retries) |

```json
{
  "apiVersion": "fleetplane.io/v1alpha1",
  "kind": "Resource",
  "metadata": {"name": "ci-large-3"},
  "spec": {
    "kind": "compute.machine",
    "provider": "hetzner-main",
    "class": "ci-large"
  }
}
```

```bash
fleetplane resources create -f machine.json --idempotency-key create-ci-large-3
```

```text
res_01J8FYKB4C7E1H9N2Q5S8T0VXD: provisioning
```

Provisioning is asynchronous — follow the create operation with
`fleetplane operations`, or watch the resource list until the phase is `ready`.

### fleetplane resources delete

Delete a resource. Deletion is drain-safe: it is refused while the resource holds
active leases (drain first, or release the acquisitions).

| Flag | Default | Description |
|---|---|---|
| `--idempotency-key` | — | Idempotency key |

```bash
fleetplane resources delete res_01J8FYK2N9V1X4T7Q0C3E6H9SD
```

```text
res_01J8FYK2N9V1X4T7Q0C3E6H9SD: deleting
```

The CLI has no dry-run flag; the API supports a delete preview via
`DELETE /v1/resources/{id}?dryRun=true`, and the [dashboard](dashboard.md) exposes it
as a delete preview.

### fleetplane resources drain

Mark a resource as draining: no new leases are placed on it, and it is deleted once
existing leases end.

```bash
fleetplane resources drain res_01J8FYK2N9V1X4T7Q0C3E6H9SD
```

```text
res_01J8FYK2N9V1X4T7Q0C3E6H9SD: draining
```

### fleetplane resources park

Stop a ready machine into the near-free parked tier
([concepts → parked machines](concepts.md#parked-machines-the-warm-tier)) —
`POST /v1/resources/{id}:park`. The stop is journaled and asynchronous —
follow the `resource.stop` operation with `fleetplane operations`, or poll
`fleetplane resources` until the phase is `parked`. On a provider that
cannot park (Hetzner, DigitalOcean) the request fails with 409 (exit code
5); a leased machine is refused the same way.

```bash
fleetplane resources park res_01J8FYK2N9V1X4T7Q0C3E6H9SD
```

```text
res_01J8FYK2N9V1X4T7Q0C3E6H9SD: parking
```

### fleetplane resources start

Start a parked machine back into service — `POST /v1/resources/{id}:start`.
The machine becomes `ready` again only after a fresh readiness probe, since
its public IP has usually changed across the stop/start cycle. Repeating
either command is safe: the API is idempotent and answers with the current
state instead of a conflict.

```bash
fleetplane resources start res_01J8FYK2N9V1X4T7Q0C3E6H9SD
```

```text
res_01J8FYK2N9V1X4T7Q0C3E6H9SD: starting
```

## fleetplane acquire

Acquire capacity: reuse an existing machine that satisfies the constraints, or create a
new one from `--class` (design docs, [08 §3](../08_HETZNER_RUNNER_USE_CASE.md)).

| Flag | Default | Description |
|---|---|---|
| `--class` | — | Resource class (needed to create new capacity) |
| `--cpu` | `0` | Minimum CPUs |
| `--memory-mib` | `0` | Minimum memory in MiB |
| `--exclusive` | `false` | Whole-machine lease (implied when no constraints are given) |
| `--ttl` | — | Lease TTL, e.g. `90m` |
| `--max-wait` | — | Queue budget, e.g. `10m`: wait for existing capacity before scaling up (needs server >= v0.4) |
| `--idempotency-key` | — | Idempotency key |

`--cpu`/`--memory-mib` become the constraints body
`{"cpu":{"min":N},"memoryMiB":{"min":N}}`. The accept response reflects the journaled
state, so the CLI immediately re-fetches the acquisition and prints its current state.

```bash
fleetplane acquire --class ci-large --cpu 4 --ttl 90m
```

```text
acq_01J8FYKF7H2K5N8Q1T4V9X0CED -> res_01J8FYK2N9V1X4T7Q0C3E6H9SD
state: bound
```

When a new machine has to be provisioned first, the state is `pending` (resource
column `-`) or `provisioning` (already showing the ID of the machine being created
for you) — use `fleetplane watch` to wait for `bound`.

## fleetplane release

Release an acquisition (retry-safe — releasing twice is not an error).

```bash
fleetplane release acq_01J8FYKF7H2K5N8Q1T4V9X0CED
```

```text
acq_01J8FYKF7H2K5N8Q1T4V9X0CED: released
```

## fleetplane watch

Poll an acquisition (`acq_…`) or operation (`op_…`) until it reaches a terminal state,
printing each state transition.

| Flag | Default | Description |
|---|---|---|
| `--interval` | `2s` | Poll interval |
| `--timeout` | `10m` | Give up after this long |

Success means `bound` for acquisitions and `succeeded` for operations. A terminal
failure (`failed`, `expired`, `released` / `failed`, `aborted`) or a timeout exits with
code 6. A queued acquisition (`fleetplane acquire --max-wait`) may legitimately stay
`pending` for up to its whole queue budget before force-scaling, so give `--timeout`
at least the `--max-wait` value plus provisioning headroom.

```bash
fleetplane watch acq_01J8FYKF7H2K5N8Q1T4V9X0CED --timeout 5m
```

```text
acq_01J8FYKF7H2K5N8Q1T4V9X0CED: provisioning
acq_01J8FYKF7H2K5N8Q1T4V9X0CED: bound -> res_01J8FYKB4C7E1H9N2Q5S8T0VXD (lease lease_01J8FYKJ2E5H8K1N4Q7T0V3XCSD)
```

```bash
fleetplane acquire --class ci-large -o json | jq -r .id | xargs fleetplane watch
```

## fleetplane apply

Apply declarative manifests: multi-document YAML (or JSON) with kinds `Pool` and
`Resource`. This is the IR-first declarative layer — it compiles onto the same
imperative API the rest of the CLI uses, never a second orchestration engine
(design docs, [04 §8](../04_API_AND_RESOURCE_MODEL.md)).

| Flag | Default | Description |
|---|---|---|
| `-f`, `--file` | (required) | Manifest file, or `-` for stdin |

Rules:

- `apiVersion`, if set, must be `fleetplane.io/v1alpha1`.
- `Pool` documents create or update the pool (keyed by name).
- `Resource` documents are **create-only**, applied with the idempotency key
  `apply:<metadata.name>` — re-applying the same file is a no-op, but `apply` will not
  mutate an existing resource.

```yaml
apiVersion: fleetplane.io/v1alpha1
kind: Pool
metadata:
  name: ci-warm
spec:
  class: ci-large
  replicas: 3
---
apiVersion: fleetplane.io/v1alpha1
kind: Resource
metadata:
  name: bastion
spec:
  kind: compute.machine
  provider: hetzner-main
  class: ci-large
```

```bash
fleetplane apply -f fleet.yaml
```

```text
pool/ci-warm (pool_01J8FYKN9Q2T5V8X1C4E7H0KSD): applied
resource/bastion (res_01J8FYKQ4V7X0C3E6H9K2N5QSD): provisioning
```

```bash
cat fleet.yaml | fleetplane apply -f -
```

## fleetplane pools

List pools, or manage one with the subcommands.

```bash
fleetplane pools
```

```text
ID                               NAME     SPEC
pool_01J8FYKN9Q2T5V8X1C4E7H0KSD  ci-warm  {"class":"ci-large","replicas":3}
```

### fleetplane pools apply

Create or update a pool from a JSON manifest (`-f` required). For YAML, use the
top-level `fleetplane apply` instead.

```json
{
  "apiVersion": "fleetplane.io/v1alpha1",
  "kind": "Pool",
  "metadata": {"name": "ci-warm"},
  "spec": {"class": "ci-large", "replicas": 3}
}
```

```bash
fleetplane pools apply -f pool.json
```

```text
ci-warm (pool_01J8FYKN9Q2T5V8X1C4E7H0KSD): applied
```

### fleetplane pools get

```bash
fleetplane pools get pool_01J8FYKN9Q2T5V8X1C4E7H0KSD
```

Prints the pool as JSON.

### fleetplane pools reconcile

Trigger reconciliation for one pool immediately instead of waiting for the periodic
reconciler (`reconcile.interval`, default `15s`).

```bash
fleetplane pools reconcile pool_01J8FYKN9Q2T5V8X1C4E7H0KSD
```

```text
pool_01J8FYKN9Q2T5V8X1C4E7H0KSD: reconciling
```

## fleetplane classes

List and manage resource classes. Config-file classes appear with source
`config` and are read-only here; API-managed classes carry source `api`.

```bash
fleetplane classes
```

```text
NAME      KIND             PROVIDER      SOURCE  RECLAIM  PARK  DELETE-AFTER  QUEUE
ci-large  compute.machine  hetzner-main  config  5m0s     auto  -             -
burst     compute.machine  gcp-main      api     10m0s    auto  4h0m0s        10m0s
```

`PARK` and `DELETE-AFTER` are the two-stage reclaim knobs for
[parked machines](concepts.md#parked-machines-the-warm-tier); `PARK` shows
`auto` when unset (the default).

### fleetplane classes create

```bash
fleetplane classes create burst \
  --provider gcp-main \
  --template '{"serverType":"e2-medium","image":"family:debian-cloud/debian-12"}' \
  --reclaim-idle-after 10m --reclaim-delete-after 4h --queue-max-wait 10m
```

| Flag | Default | Description |
|---|---|---|
| `--kind` | `compute.machine` | Resource kind |
| `--provider` | — | Provider instance name (required) |
| `--template` | — | Kind-specific spec as inline JSON |
| `--template-file` | — | Kind-specific spec from a JSON file |
| `--reclaim-idle-after` | — | Idle reclamation policy, e.g. `5m` |
| `--reclaim-park` | — | Stage-1 disposition on park-capable providers: `auto` (default) or `never` |
| `--reclaim-delete-after` | — | Delete machines parked this long, e.g. `4h` (poolless classes only) |
| `--queue-max-wait` | — | Acquisition queue budget, e.g. `10m` |

The template is validated against the kind registry at write time.

### fleetplane classes get / delete

```bash
fleetplane classes get burst      # JSON envelope
fleetplane classes delete burst   # refused (409) while a pool references it
```

Classes can also be applied declaratively — `fleetplane apply -f` accepts
`kind: Class` manifests (upsert by name); put Class documents before the
Pool documents that reference them.

## fleetplane operations

List open (non-terminal) operations, or show one as JSON.

```bash
fleetplane operations
```

```text
ID                             KIND             STATE      RESOURCE                        ATTEMPT  ERROR
op_01J8FYKS8E1H4K7N0Q3T6V9XSD  resource.create  in_flight  res_01J8FYKQ4V7X0C3E6H9K2N5QSD  1
op_01J8FYKV2C5E8H1K4N7Q0T3VSD  resource.delete  uncertain  res_01J8FYK7X2T4V9N1Q5C8E3H6SD  4        retryable
```

```bash
fleetplane operations op_01J8FYKS8E1H4K7N0Q3T6V9XSD
```

Operation states are described in [ADR-017](../adr/ADR-017-operation-states.md). An
operation stuck in `uncertain` needs an operator decision; resolve it via the API
(`POST /v1/operations/{id}:resolve` with `{"action":"retry-verification"}` or
`{"action":"mark-failed"}`) or from the [dashboard](dashboard.md) — there is no CLI
subcommand for it.

## fleetplane events

List audit events in chronological order (ULID IDs sort by time).

| Flag | Default | Description |
|---|---|---|
| `--since` | — | Look-back window, e.g. `1h` |
| `--after` | — | Cursor: return events after this `evt_…` ID |
| `--limit` | `100` | Maximum events to return (the server caps it at 1000) |

```bash
fleetplane events --since 1h
```

```text
ID                              TIME      TYPE               OUTCOME    RESOURCE                        ACTOR
evt_01J8FYKX6H9K2N5Q8T1V4X7CSD  09:12:44  resource.create    journaled  res_01J8FYKB4C7E1H9N2Q5S8T0VXD  ci
evt_01J8FYKZ1E4H7K0N3Q6T9V2XSD  09:14:02  acquisition.bound  bound      res_01J8FYKB4C7E1H9N2Q5S8T0VXD  ci
```

Paginate by passing the last ID of a page as `--after`.

## fleetplane providers

Show provider instance health (design docs,
[07 §8](../07_SECURITY_AND_OPERATIONS.md)). Provider health never affects `/health/ready`.

```bash
fleetplane providers
```

```text
INSTANCE      DRIVER        STATE     FAILURES  LAST ERROR
hetzner-main  hetzner       healthy   0
do-backup     digitalocean  degraded  3         Get "https://api.digitalocean.com/v2/account": context deadline exceeded
```

## fleetplane token new

Generate an API token **locally** — no server call, no server state. It prints the
plaintext secret exactly once, plus a ready-to-paste `auth.tokens` snippet for the
server config; the config stores only the SHA-256 of the secret
([ADR-008](../adr/ADR-008-tokens.md)).

| Flag | Default | Description |
|---|---|---|
| `--name` | `default` | Token name (shown in audit events) |
| `--perm` | `admin` | Permission, repeatable |

```bash
fleetplane token new --name ci --perm resource.read --perm resource.acquire --perm operation.read
```

```text
token: flp_8f3a1c2d.Zkw3vWQx9pT4hK2mN8rB5cD1eF6gH0jL3nP7qS9uVaX

add to fleetplane config:

auth:
  tokens:
    - id: 8f3a1c2d
      name: ci
      sha256: 9c2f0e8a7b6d5c4e3f2a1b0c9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e
      permissions: [resource.read, resource.acquire, operation.read]
```

Valid permissions (closed set): `resource.read`, `resource.acquire`, `resource.create`,
`resource.delete`, `pool.read`, `pool.write`, `provider.read`, `provider.admin`,
`operation.read`, `admin` (allows everything). Unknown permissions are rejected — by
`token new` and again at server boot.

With no tokens configured, the server runs with authentication **disabled** (open API,
loud boot warning) and the CLI needs no `--token`.

## fleetplane admin backup

Trigger a hot backup (`VACUUM INTO`, safe under WAL) via the **ops listener** — the
documented exception to "the CLI only speaks the public API"
([ADR-006](../adr/ADR-006-cli.md)). The destination path is interpreted **on the
server host**, not the machine running the CLI.

| Flag | Default | Description |
|---|---|---|
| `--to` | (required) | Destination path on the server host |
| `--ops-addr` | `$FLEETPLANE_OPS_ADDR`, else `http://127.0.0.1:9090` | Ops listener address |

```bash
fleetplane admin backup --to /var/backups/fleetplane-2026-08-15.db
```

```text
backup written to /var/backups/fleetplane-2026-08-15.db
```

The HTTP client allows up to 10 minutes — `VACUUM INTO` scales with database size. The
ops listener binds to loopback by default, so run this on the server host (or through
an SSH tunnel). See the [backup and restore runbook](../runbooks/backup-restore.md).
