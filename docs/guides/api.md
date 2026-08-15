# HTTP API

Fleetplane exposes a single JSON-over-HTTP API. Everything else — the `fleetplane` CLI, the embedded web dashboard, the Go client in `pkg/apiclient` — is a client of this API; there is no privileged side channel.

The machine-readable contract is [`api/openapi.yaml`](../../api/openapi.yaml) (OpenAPI 3.0.3, hand-authored, enforced by a contract test — see [ADR-011](../adr/ADR-011-openapi.md)). This guide is the human companion. For the resource model behind the API, see the [API and resource model design doc](../04_API_AND_RESOURCE_MODEL.md).

## Base URL and conventions

The API listens on `server.addr` (default `:8080`). All API routes live under `/v1/`. The version string inside envelopes is `fleetplane.io/v1alpha1`.

```bash
BASE=http://127.0.0.1:8080
TOKEN=flp_1a2b3c4d.your-secret-here   # see Authentication
```

Conventions that apply to every route:

- **Content type** — requests and responses are `application/json`.
- **Request IDs** — every response carries an `X-Request-Id` header. Send your own and it is echoed back; otherwise the server generates one (`req_` + 16 hex characters). Error bodies repeat it as `requestId`.
- **Body limit** — request bodies on the create and update endpoints (resources, acquisitions, pools) are capped at 1 MiB.
- **Strict decoding** — unknown JSON fields are rejected (HTTP 400) on `POST /v1/resources` and `POST /v1/acquisitions`.
- **Colon verbs** — actions that are not plain CRUD are spelled as `POST /v1/<collection>/{id}:<verb>` (`:drain`, `:reconcile`, `:resolve`). The path is split at the *last* colon; IDs never contain one ([ADR-005](../adr/ADR-005-http-router.md)). An unknown verb returns 404 `not_found`.
- **IDs** — every entity ID is a prefixed ULID: `res_`, `pool_`, `acq_`, `lease_`, `op_`, `evt_` ([ADR-004](../adr/ADR-004-ids.md)).
- **Async accept** — mutations that touch a provider (create, delete) return `201`/`202` as soon as the intent is durably journaled. The operation engine converges in the background; poll the resource, acquisition, or operation to observe progress.

## Authentication

Fleetplane uses static bearer tokens ([ADR-008](../adr/ADR-008-tokens.md)):

```text
Authorization: Bearer flp_<id8>.<secret>
```

`<id8>` is an 8-hex-character lookup ID; `<secret>` is a base64url-encoded 256-bit random value. The server stores only `hex(sha256(secret))` in `auth.tokens` and compares in constant time.

Mint a token locally (no server call) and paste the printed YAML snippet into your config:

```bash
fleetplane token new --name ci --perm resource.acquire --perm resource.read
```

If **no tokens are configured**, authentication is disabled and the API runs open — for local development only. The server logs a loud warning at boot.

Failure modes:

- Missing/invalid token → `401` with code `unauthenticated` and a `WWW-Authenticate: Bearer` header.
- Valid token, missing permission → `403` with code `permission_denied` (message names the token and the missing permission).

### Permissions

Each route requires exactly one permission (see the route table below). The closed set:

| Permission | Grants |
|---|---|
| `resource.read` | Read resources and acquisitions |
| `resource.acquire` | Acquire and release capacity |
| `resource.create` | Create resources directly |
| `resource.delete` | Delete and drain resources |
| `pool.read` | Read pools |
| `pool.write` | Create, update, reconcile pools |
| `provider.read` | Read provider health |
| `provider.admin` | Resolve uncertain operations |
| `operation.read` | Read operations and events |
| `admin` | Everything |

## Route table

The server is driven by a single route table that also drives per-route authorization and the OpenAPI contract test.

| Method | Path | Permission | Mutating |
|---|---|---|---|
| POST | `/v1/acquisitions` | `resource.acquire` | yes |
| GET | `/v1/acquisitions/{id}` | `resource.read` | no |
| DELETE | `/v1/acquisitions/{id}` (release) | `resource.acquire` | yes |
| POST | `/v1/resources` | `resource.create` | yes |
| GET | `/v1/resources` | `resource.read` | no |
| GET | `/v1/resources/{id}` | `resource.read` | no |
| DELETE | `/v1/resources/{id}` | `resource.delete` | yes |
| POST | `/v1/resources/{id}:drain` | `resource.delete` | yes |
| POST | `/v1/pools` | `pool.write` | yes |
| GET | `/v1/pools` | `pool.read` | no |
| GET | `/v1/pools/{id}` | `pool.read` | no |
| PUT | `/v1/pools/{id}` | `pool.write` | yes |
| POST | `/v1/pools/{id}:reconcile` | `pool.write` | yes |
| GET | `/v1/operations` | `operation.read` | no |
| GET | `/v1/operations/{id}` | `operation.read` | no |
| POST | `/v1/operations/{id}:resolve` | `provider.admin` | yes |
| GET | `/v1/events` | `operation.read` | no |
| GET | `/v1/providers` | `provider.read` | no |

The read endpoints beyond the original design (acquisition get, pool reads, operation list, providers, `:resolve`) are documented in [ADR-API-001](../adr/ADR-API-001-read-endpoints.md).

## Envelopes

The wire shapes live in `pkg/apiclient` — the server serializes exactly those Go types, so the package doubles as the reference Go client.

### Resource

```json
{
  "apiVersion": "fleetplane.io/v1alpha1",
  "kind": "Resource",
  "metadata": {
    "id": "res_01J5X0A9GVQ4C2E7NBM8KTWZSD",
    "name": "runner-001",
    "labels": { "team": "ci" },
    "ownership": "managed",
    "generation": 1,
    "observedGeneration": 1,
    "createdAt": "2026-08-15T12:00:00Z",
    "updatedAt": "2026-08-15T12:00:41Z"
  },
  "spec": {
    "kind": "compute.machine",
    "provider": "hetzner-main",
    "class": "ci-large",
    "machine": { "serverType": "cpx31", "image": "name:ubuntu-24.04" }
  },
  "status": {
    "phase": "ready",
    "externalId": "104793211",
    "externalRef": { "id": "104793211" },
    "capacity": { "cpu": 4, "memoryMiB": 8192 },
    "extensions": { "...": "provider-native object, passed through untouched" }
  }
}
```

Notes:

- `metadata.ownership` is `managed`, `adopted`, or `observed`; `metadata.protected` marks delete-protected resources.
- `status.phase` is one of `unknown`, `provisioning`, `ready`, `allocated`, `draining`, `deleting`, `failed`, `orphaned`. There is no `deleted` phase — deletion terminality is the storage tombstone, surfaced as `metadata.deletedAt` ([ADR-017](../adr/ADR-017-operation-states.md)).
- `status.extensions` is the provider's native object, passed through without translation.

### Acquisition

```json
{
  "apiVersion": "fleetplane.io/v1alpha1",
  "kind": "Acquisition",
  "id": "acq_01J5X0B2M4T8RDWQ6YHF3KPVNC",
  "state": "bound",
  "class": "ci-large",
  "resourceKind": "compute.machine",
  "resourceId": "res_01J5X0A9GVQ4C2E7NBM8KTWZSD",
  "leaseId": "lease_01J5X0B6H9ZS3XKD7QGWMRT2FE",
  "actor": "ci",
  "createdAt": "2026-08-15T12:01:00Z",
  "updatedAt": "2026-08-15T12:01:04Z"
}
```

`state` is one of `pending`, `provisioning`, `bound`, `failed`, `released`, `expired`. `resourceId` appears as soon as the acquisition is bound to a resource — on the scale-on-demand path it is already set while the acquisition is `provisioning` (the pre-bound pending resource); `leaseId` appears once the acquisition is `bound`.

### Pool

```json
{
  "apiVersion": "fleetplane.io/v1alpha1",
  "kind": "Pool",
  "metadata": { "id": "pool_01J5X0C8T2VBKQ4WYNRD6HZMGS", "name": "ci-runners", "generation": 1, "createdAt": "2026-08-15T12:02:00Z", "updatedAt": "2026-08-15T12:02:00Z" },
  "spec": { "class": "ci-large", "replicas": 4, "minReady": 1, "maxResources": 20, "reclaim": { "idleAfter": "10m" } }
}
```

The pool `spec` must name either a configured `class`, or an inline template of `kind` + `provider` + `machine`. `replicas` must be >= 0. A top-level `"paused": true` field appears when the pool is paused (it is omitted otherwise).

### Operation

```json
{
  "id": "op_01J5X0D4N7WCFT2QKYHB8VZRGM",
  "kind": "create",
  "state": "uncertain",
  "provider": "hetzner-main",
  "resourceId": "res_01J5X0A9GVQ4C2E7NBM8KTWZSD",
  "attempt": 3,
  "errorClass": "retryable",
  "createdAt": "2026-08-15T12:03:00Z",
  "updatedAt": "2026-08-15T12:07:12Z"
}
```

Operation states: `journaled` → `in_flight` → `external_accepted` → `verifying` → `uncertain` (frozen for manual `:resolve`); terminal states are `succeeded`, `failed`, `aborted` ([ADR-017](../adr/ADR-017-operation-states.md)). `GET /v1/operations` lists open (non-terminal) operations.

### Event

```json
{
  "id": "evt_01J5X0E9K3RHXW7QT2NBVGZMCD",
  "ts": "2026-08-15T12:01:00Z",
  "type": "acquisition.requested",
  "actor": "ci",
  "outcome": "pending",
  "resourceId": "res_01J5X0A9GVQ4C2E7NBM8KTWZSD"
}
```

### Lists

List endpoints wrap items in a list envelope:

```json
{ "apiVersion": "fleetplane.io/v1alpha1", "kind": "ResourceList", "items": [] }
```

The kinds are `ResourceList`, `PoolList`, `OperationList`, `EventList`, and `ProviderList`. Provider items look like:

```json
{ "instance": "hetzner-main", "driver": "hetzner", "state": "healthy", "since": "2026-08-15T11:00:00Z", "lastCheck": "2026-08-15T12:07:00Z", "consecutiveFailures": 0 }
```

Provider health states: `unknown`, `healthy`, `degraded`, `unavailable`. Provider health is surfaced here — never through the readiness probe.

## Errors

Every non-2xx response carries one shape:

```json
{
  "error": {
    "code": "conflict",
    "message": "resource res_01J5X0A9... has 1 active lease(s) (invariant 3)",
    "requestId": "req_9f2c1ab34de56f78",
    "retryable": false
  }
}
```

`retryAfterSeconds` and `details` appear when relevant. Codes and their HTTP status:

| Code | HTTP | Meaning |
|---|---|---|
| `invalid` | 400 | Malformed body, unknown fields, failed validation |
| `unauthenticated` | 401 | Missing or invalid bearer token (+ `WWW-Authenticate: Bearer`) |
| `permission_denied` | 403 | Token lacks the route's permission |
| `not_found` | 404 | Unknown ID, tombstoned resource, or unknown colon verb |
| `conflict` | 409 | State conflict — e.g. deleting a leased or protected resource |
| `idempotency_in_flight` | 409 | Original request for this key still running; `details.operationId` names the operation to poll |
| `idempotency_mismatch` | 409 | Idempotency key reused with a different request body |
| `internal` | 500 | Server error (`retryable: true`, except panic recovery which is `retryable: false`) |
| `unready` | 503 | Mutation gate: recovery in progress or shutting down (`retryable: true`, `Retry-After: 2`) |

## Idempotency

Send an `Idempotency-Key` header on the mutating endpoints that journal provider work:

- `POST /v1/resources`
- `DELETE /v1/resources/{id}`
- `POST /v1/acquisitions` — the key may also be passed in the body as `idempotencyKey`; if both header and body are set they must agree, or the request fails with 400 `invalid`.

Semantics:

- **Replay** — retrying a completed request with the same key returns the original status and the *stored response bytes verbatim*, plus an `Idempotency-Replayed: true` header. Completed outcomes replay for 48 hours.
- **Scope** — a key is scoped to `"<METHOD> <collection path>|<actor>"`, where the actor is the token name — e.g. `"POST /v1/acquisitions|ci"`. A delete's scope is `"DELETE /v1/resources|<actor>"` (the resource ID is folded into the request hash, not the scope). Different tokens (or different endpoints) never collide on the same key string.
- **In flight** — retrying while the original request is still running returns 409 `idempotency_in_flight` with `details.operationId`; poll that operation instead of retrying blindly.
- **Mismatch** — reusing a key with a different request body (the server compares a SHA-256 of the body) returns 409 `idempotency_mismatch`.

`DELETE /v1/acquisitions/{id}` (release) takes no key — it is naturally retry-safe: releasing an already-released or expired acquisition is a no-op success. Dry-run deletes are read-only and are not recorded against the key.

## Query parameters

**`GET /v1/resources`** — optional filters, combinable:

| Param | Filters on |
|---|---|
| `kind` | Resource kind, e.g. `compute.machine` |
| `class` | Class name |
| `provider` | Provider instance name |

**`GET /v1/events`** — the audit trail; combine a cursor or lookback with a limit:

| Param | Meaning |
|---|---|
| `after` | Return events after this `evt_` ID (cursor) |
| `since` | Go duration lookback, e.g. `1h`, `30m` |
| `limit` | Max items; default 100, capped at 1000 |

```bash
curl -sS -H "Authorization: Bearer $TOKEN" \
  "$BASE/v1/events?since=1h&limit=200"
```

**`DELETE /v1/resources/{id}?dryRun=true`** — runs every deletion gate (managed ownership, not protected, no active leases) and reports the plan without mutating anything:

```bash
curl -sS -X DELETE -H "Authorization: Bearer $TOKEN" \
  "$BASE/v1/resources/res_01J5X0A9GVQ4C2E7NBM8KTWZSD?dryRun=true"
```

```json
{ "id": "res_01J5X0A9GVQ4C2E7NBM8KTWZSD", "wouldDelete": true, "dryRun": true }
```

A dry run returns 200; a real delete returns 202 with `{"id": "...", "status": "deleting"}`. If a gate fails, both return the same error a real delete would (409 `conflict` for a leased or protected resource, 400 `invalid` for non-managed ownership).

## Health, readiness, and the mutation gate

Both listeners serve the health endpoints:

- `GET /health/live` — always 200 while the process runs.
- `GET /health/ready` — 200 only after storage responds, journal recovery has completed, and acquisition resume has finished. Until then (and if storage becomes unreachable) it returns 503 with `{"status": "unready"}`. Provider health **never** affects readiness — a degraded cloud provider does not make Fleetplane unready; check `GET /v1/providers` instead.

While the server is not ready — during boot recovery and during shutdown drain — a **mutation gate** rejects every non-GET/HEAD request under `/v1/` with 503:

```json
{ "error": { "code": "unready", "message": "recovery in progress or shutting down", "retryable": true } }
```

The response carries `Retry-After: 2`. Reads always pass the gate. Clients should treat `unready` as "retry shortly", not as failure.

## Ops listener

A second listener on `server.opsAddr` (default `127.0.0.1:9090`, keep it loopback) serves operational endpoints outside the API surface:

- `GET /metrics` — Prometheus metrics
- `GET /debug/pprof/` (plus `profile`, `symbol`, `trace`) — Go profiling
- `POST /admin/backup` with body `{"to": "/abs/path.db"}` — online SQLite backup via `VACUUM INTO`; see the [backup and restore runbook](../runbooks/backup-restore.md)

## Example flows

### Create a resource

```bash
curl -sS -X POST "$BASE/v1/resources" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: create-runner-001" \
  -d '{
    "metadata": { "name": "runner-001", "labels": { "team": "ci" } },
    "spec": {
      "kind": "compute.machine",
      "provider": "hetzner-main",
      "machine": { "serverType": "cpx31", "image": "name:ubuntu-24.04", "location": "fsn1" }
    }
  }'
```

The 201 response is the Resource envelope with `status.phase: "provisioning"` — the create is journaled and the engine converges it asynchronously. Poll until ready:

```bash
curl -sS -H "Authorization: Bearer $TOKEN" "$BASE/v1/resources/res_01J5X0A9GVQ4C2E7NBM8KTWZSD"
```

Retrying the POST with the same `Idempotency-Key` returns the original 201 body byte-for-byte with `Idempotency-Replayed: true`.

### Acquire and release capacity

Acquire asks Fleetplane to *find or create* suitable capacity: it prefers an existing ready resource with enough free capacity, and scales on demand otherwise. A request needs a `class` and/or `constraints`; `kind` defaults to `compute.machine`; `quantity` must be 1 in v1.

```bash
curl -sS -X POST "$BASE/v1/acquisitions" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: job-4211" \
  -d '{
    "class": "ci-large",
    "constraints": { "cpu": { "min": 2 }, "memoryMiB": { "min": 4096 } },
    "exclusive": true,
    "lease": { "ttl": "90m" }
  }'
```

The 201 body reflects the journaled state (usually `"state": "pending"`). Poll until it is `bound` — the response then carries `resourceId` and `leaseId`:

```bash
curl -sS -H "Authorization: Bearer $TOKEN" "$BASE/v1/acquisitions/acq_01J5X0B2M4T8RDWQ6YHF3KPVNC"
```

Terminal failure states are `failed` (no capacity and nothing to scale from) and `expired` (never satisfied within the pending timeout). When done, release — releasing twice is a no-op success:

```bash
curl -sS -X DELETE -H "Authorization: Bearer $TOKEN" \
  "$BASE/v1/acquisitions/acq_01J5X0B2M4T8RDWQ6YHF3KPVNC"
```

```json
{ "id": "acq_01J5X0B2M4T8RDWQ6YHF3KPVNC", "state": "released" }
```

The lease TTL is a safety net: if you crash before releasing, the lease expires on its own.

### Create or update a pool

`POST /v1/pools` upserts by `metadata.name` — posting an existing name updates that pool. `PUT /v1/pools/{id}` updates by ID. Both return 200 with the Pool envelope and kick the reconciler.

```bash
curl -sS -X POST "$BASE/v1/pools" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "kind": "Pool",
    "metadata": { "name": "ci-runners" },
    "spec": {
      "class": "ci-large",
      "replicas": 4,
      "minReady": 1,
      "maxResources": 20,
      "reclaim": { "idleAfter": "10m" }
    }
  }'
```

`replicas` is a convergence target, not a one-shot scale command — the reconciler continuously converges toward it. Trigger an immediate pass:

```bash
curl -sS -X POST -H "Authorization: Bearer $TOKEN" \
  "$BASE/v1/pools/pool_01J5X0C8T2VBKQ4WYNRD6HZMGS:reconcile"
```

```json
{ "id": "pool_01J5X0C8T2VBKQ4WYNRD6HZMGS", "status": "reconciling" }
```

### Resolve an uncertain operation

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

## Web dashboard

`fleetplane serve` also serves an embedded web dashboard on the same listener at `http://<server.addr>/ui/` (`/` redirects there). It is built into the binary — no extra deployment. Log in by pasting an API token; with no tokens configured the API is open and no token is needed. The dashboard covers the same surface as this API: resources, pools, acquisitions, operations, events, and provider health, including delete-with-dry-run and uncertain-operation resolution. See the [dashboard guide](dashboard.md).

## Go client

`github.com/samimishal/fleetplane/pkg/apiclient` is a minimal Go client over this API:

```go
package main

import (
	"context"
	"fmt"

	"github.com/samimishal/fleetplane/pkg/apiclient"
)

func main() {
	c := apiclient.New("http://127.0.0.1:8080", "flp_1a2b3c4d.your-secret-here")
	acq, err := c.Acquire(context.Background(), apiclient.AcquireRequest{
		Class: "ci-large",
		Lease: &apiclient.LeaseRequest{TTL: "90m"},
	}, "job-4211")
	if err != nil {
		panic(err)
	}
	fmt.Println(acq.ID, acq.State)
}
```

Non-2xx responses come back as `*apiclient.APIError` carrying the decoded error detail.

## See also

- [`api/openapi.yaml`](../../api/openapi.yaml) — the machine-readable contract
- [API and resource model](../04_API_AND_RESOURCE_MODEL.md) design doc — the model behind the routes
- [Security and operations](../07_SECURITY_AND_OPERATIONS.md) design doc — permissions, ops listener, readiness philosophy
- [ADR-008](../adr/ADR-008-tokens.md) — token format · [ADR-005](../adr/ADR-005-http-router.md) — colon verbs · [ADR-011](../adr/ADR-011-openapi.md) — OpenAPI contract testing · [ADR-017](../adr/ADR-017-operation-states.md) — operation states · [ADR-API-001](../adr/ADR-API-001-read-endpoints.md) — additive read endpoints
- [Dashboard guide](dashboard.md) — the embedded web UI
