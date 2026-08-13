# Security and Operations

## 1. Threat model

Fleetplane holds credentials capable of creating and destroying infrastructure. Treat it as privileged infrastructure.

## 2. Authentication

v1:
- static API tokens with hashed storage, or external reverse-proxy auth;
- scoped tokens where feasible.

Future:
- OIDC;
- mTLS service identities;
- workload identity.

## 3. Authorization

Suggested permissions:

```text
resource.read
resource.acquire
resource.create
resource.delete
pool.read
pool.write
provider.read
provider.admin
operation.read
admin
```

## 4. Secrets

Provider credentials should be loaded through secret references:

```text
secret://env/HETZNER_TOKEN
secret://file/run/secrets/hetzner
```

Future resolvers may support cloud secret managers. Never return resolved credentials through API responses.

## 5. Ownership and deletion safety

Every resource should have an ownership mode:

- `managed`: Fleetplane may mutate/delete.
- `adopted`: Fleetplane manages according to explicit policy.
- `observed`: read-only.

Deletion should require both resource ownership and policy authorization.

## 6. Audit

Record:
- actor;
- request/idempotency key;
- intent;
- generated plan;
- provider operations;
- final outcome;
- timestamps;
- external request IDs.

## 7. Observability

Metrics:
- resources by provider/kind/phase;
- ready/free capacity;
- active/pending leases;
- reconcile duration;
- provider request latency/errors/rate limits;
- operation queue depth;
- create-to-ready latency;
- idle resource count;
- estimated spend later.

Logs should carry:
`request_id`, `operation_id`, `resource_id`, `pool_id`, `provider`.

## 8. Health endpoints

```text
/health/live
/health/ready
/metrics
```

Readiness should fail when required storage is unavailable. A degraded provider should be surfaced separately rather than necessarily making the whole API unready.

## 9. Safe shutdown

- stop accepting new mutations;
- finish or checkpoint in-flight DB transactions;
- persist operation state;
- release local locks;
- do not assume provider operations stop when the process exits.
