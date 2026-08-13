# Provider and Module SDK

## 1. Design

A provider represents an external infrastructure authority: Hetzner Cloud, GCP, AWS, DigitalOcean, a local hypervisor, or another system.

A **resource kind** represents semantics. A **provider implementation** states which kinds/capabilities it supports.

This split is important: `compute.machine` is not Hetzner, and Hetzner is not limited to machines.

## 2. Proposed Go interfaces

```go
type Provider interface {
    Descriptor() ProviderDescriptor
    Capabilities(ctx context.Context) ([]Capability, error)
    ResourceDriver(kind ResourceKind) (ResourceDriver, bool)
    Health(ctx context.Context) error
}

type ResourceDriver interface {
    Kind() ResourceKind

    Discover(ctx context.Context, req DiscoverRequest) ([]ObservedResource, error)
    Get(ctx context.Context, ref ExternalRef) (ObservedResource, error)

    Plan(ctx context.Context, req PlanRequest) (Plan, error)
    Apply(ctx context.Context, action Action) (OperationRef, error)
    ObserveOperation(ctx context.Context, op OperationRef) (OperationStatus, error)
}
```

The exact interface should remain small. Optional functionality is exposed through capabilities rather than continuously expanding one interface.

## 3. Capability pattern

Examples:

```text
compute.machine.create
compute.machine.resize
compute.machine.power
compute.machine.snapshot
compute.machine.metrics
compute.machine.network.attach
database.postgres.create
resource.labels
resource.cost.estimate
operation.cancel
```

Core logic can ask whether a capability exists and choose a strategy accordingly.

## 4. Provider configuration

A provider instance is a configured use of a provider implementation:

```yaml
providers:
  hetzner-prod:
    driver: hetzner
    credentials:
      token: secret://env/HETZNER_TOKEN
    defaults:
      location: fsn1
```

Credentials must be references, not values persisted in ordinary configuration/state records.

## 5. Compile-time registration

Initial model:

```go
func init() {
    provider.Register("hetzner", New)
}
```

Custom distributions import desired modules and produce one static binary. A build helper can later provide:

```text
fleetplane build   --with github.com/fleetplane/provider-hetzner   --with github.com/acme/provider-internal
```

This mirrors the operational simplicity of custom Caddy/CoreDNS builds without depending on Go's runtime `plugin` package.

## 6. Provider contract requirements

Providers MUST:

- map external resources to stable external IDs;
- make create requests idempotent where the upstream API allows it;
- otherwise participate in Fleetplane's operation journal/idempotency strategy;
- distinguish retryable, terminal, rate-limit and conflict errors;
- expose asynchronous operations explicitly;
- never silently reinterpret unsupported constraints;
- return provider-native metadata through extension fields;
- support deterministic discovery of Fleetplane-owned resources where possible.

## 7. Error model

```go
type ErrorClass string

const (
    ErrRetryable   ErrorClass = "retryable"
    ErrRateLimited ErrorClass = "rate_limited"
    ErrConflict    ErrorClass = "conflict"
    ErrNotFound    ErrorClass = "not_found"
    ErrInvalid     ErrorClass = "invalid"
    ErrQuota       ErrorClass = "quota"
    ErrTerminal    ErrorClass = "terminal"
)
```

Errors should include retry-after, provider request ID and safe diagnostic metadata.

## 8. Conformance suite

The SDK should ship tests that provider authors can run for:

- discovery stability;
- create/delete lifecycle;
- idempotency behavior;
- cancellation semantics;
- pagination;
- rate limiting;
- resource label ownership;
- eventual-consistency handling;
- not-found behavior;
- operation polling.
