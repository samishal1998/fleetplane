# Storage and High Availability

## 1. v1 recommendation: SQLite

SQLite is appropriate for the first single-control-plane deployment:

- no external service;
- transactional;
- easy backup;
- ideal for a tiny always-on node;
- sufficient for moderate control-plane write volume.

Use WAL mode and explicit migrations.

## 2. Storage interface

Do not expose SQL details to orchestration code.

```go
type Store interface {
    Resources() ResourceStore
    Pools() PoolStore
    Leases() LeaseStore
    Operations() OperationStore
    Events() EventStore
    Idempotency() IdempotencyStore
    Tx(ctx context.Context, fn func(TxStore) error) error
}
```

Avoid designing the interface as generic key/value storage. Fleetplane has domain transactions and indexes.

## 3. Minimum persisted entities

- provider instances/config references;
- resource records;
- desired pool specs;
- observed snapshots;
- leases/allocations;
- operation journal;
- idempotency records;
- events/audit records;
- controller checkpoints.

## 4. Operation journal

Before a destructive or create mutation:

```text
1. transaction: record intended action
2. commit
3. call provider
4. persist external operation/reference
5. observe until terminal
6. persist result
```

After a crash, incomplete journal entries are resumed or reconciled.

## 5. Why not etcd first

etcd is excellent for distributed coordination, but requiring it would defeat the tiny-single-node deployment goal. Fleetplane should not copy Kubernetes architecture where the requirements differ.

## 6. HA path

Recommended progression:

### Stage 1
SQLite + one process.

### Stage 2
Postgres backend + multiple stateless API replicas + one elected reconciler or distributed work leases.

### Stage 3
Optional etcd/consensus-oriented backend only if there is a concrete requirement for low-latency distributed coordination.

The storage abstraction must support this evolution without pretending SQLite and etcd have identical semantics.

## 7. Leadership

When HA is introduced, use explicit controller leadership/work leases. Do not allow two reconcilers to independently decide to satisfy the same deficit.

## 8. Backups

SQLite:
- online backup API or safe snapshot process;
- backup state before upgrades;
- state should still be reconstructable from provider discovery where possible.

Provider resources must never become undeletable merely because local state was lost.
