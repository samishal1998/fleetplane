# ADR-013: Provider registration and reserved label keys

Status: Accepted (2026-08-13)

## Registration

`pkg/sdk/provider.Register(driver, factory)` from provider `init()`; `internal/boot` snapshots the registry once at startup. `cmd/fleetplane/modules.go` is the only file that blank-imports providers — adding a provider touches no central switch (PRD success metric).

## Reserved labels (exported constants in `pkg/sdk/provider`)

```
fleetplane.io/managed = "true"
fleetplane.io/owner   = <OwnerID>            # control-plane identity; persisted, boot refuses mismatch
fleetplane.io/id      = <res_ULID>
fleetplane.io/op      = <op_ULID>            # create-dedup anchor: ActionID := OperationID
fleetplane.io/test, fleetplane.io/test-run   # E2E only (ADR-015)
```

The op label is the sole create-dedup anchor (it distinguishes retry attempts, which a resource-id label cannot). Identity labels are kernel-composed into `DesiredState.Labels`; drivers apply them verbatim. Label-selector `Discover` is mandatory for v1 drivers; a driver that cannot support it declares `SupportsLabelDiscovery=false` and its uncertain operations freeze for manual resolution instead of auto-resolving.
