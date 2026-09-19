---
title: "Writing a provider"
description: "The provider SDK contract, optional billing and parking capabilities, the op-label dedup rule, error classification, registration and the conformance kit."
---

A provider is a Go package implementing two interfaces from
[`pkg/sdk/provider`](https://github.com/samishal1998/fleetplane/tree/main/pkg/sdk/provider),
registered via `init()`, and proven by the conformance suite. The fake provider
([`providers/fake`](https://github.com/samishal1998/fleetplane/tree/main/providers/fake))
is the reference implementation — small, complete, and exercised by every kernel
test. Read it first; the design rationale is in the
[Provider SDK design doc](https://github.com/samishal1998/fleetplane/blob/main/docs/03_PROVIDER_SDK.md).

## Boundary rules

Enforced mechanically by `make boundaries` ([ADR-009](/fleetplane/developers/adr/adr-009-boundaries/)):

- Provider packages may import `pkg/sdk/...`, the kind packages (`pkg/kinds/...`),
  and their cloud SDK — **never** `internal/...`.
- The kernel (`internal/...`, `pkg/...`) never imports provider packages or
  cloud SDKs.
- `pkg/sdk` itself is stdlib-only, so out-of-tree providers depend on nothing
  but the SDK surface.

In-tree drivers also share the rate pacer in
[`providers/pacing`](https://github.com/samishal1998/fleetplane/tree/main/providers/pacing).

## The contract

```go
// pkg/sdk/provider
type Provider interface {
    Descriptor() Descriptor
    Capabilities(ctx context.Context) ([]CapabilityID, error)
    ResourceDriver(kind ResourceKind) (ResourceDriver, bool)
    Health(ctx context.Context) error
    Close() error
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

One `Provider` serves one configured instance and may drive several kinds; each
`ResourceDriver` drives one kind. Implementations must be safe for concurrent
use. The lifecycle of every mutation is:

1. **Plan** — pure. Given desired state (or `nil` for "converge to absence") and
   the last observation, return the ordered `Action`s that converge them. No
   provider calls that mutate, no side effects. Actions must JSON round-trip
   exactly: `Apply` receives precisely what the journal persisted, possibly
   after a crash, with no in-memory context.
2. **Apply** — execute one journaled action. Return an `OperationRef` whose
   `Ref` (the external ID) is set **as soon as the provider assigns identity**;
   the kernel persists it immediately, before the operation finishes.
3. **ObserveOperation** — poll the operation to a terminal state, **by
   reference, never by listing**. `RetryAfter` hints the next poll interval.
   `OperationRef.Data` is driver-private resume state and may be lost across
   crashes — tolerate `Data == nil` by degrading to resource-status observation.
4. **Discover / Get** — `Discover` lists (fully paginated internally), honoring
   `ScopeOwned` (only resources carrying this control plane's ownership labels)
   and label-equality selectors. `Get` fetches by external ref; a missing
   resource returns `*Error{Class: ErrNotFound}` — never a zero value with a
   nil error.

Every observation fills `ObservedResource.Extensions` with the full native
object verbatim (invariant 6) — Fleetplane passes it through to
`status.extensions` untouched.

## Optional capability: billing

A driver whose cloud bills in coarse increments can say so per kind by
implementing the optional `provider.BillingAware` interface
([`pkg/sdk/provider/billing.go`](https://github.com/samishal1998/fleetplane/blob/main/pkg/sdk/provider/billing.go)),
discovered by type assertion — no change to the core `Provider` contract:

```go
type BillingAware interface {
    Billing(kind ResourceKind) BillingPolicy
}

type BillingPolicy struct {
    MinimumDuration   time.Duration // shortest period ever billed (0 = none)
    BillingIncrement  time.Duration // granularity after the minimum (0 = fine-grained)
    TerminationBuffer time.Duration // dispatch deletes this early before a boundary
}
```

The zero `BillingPolicy` means fine-grained billing — no billing-window
behavior — and `Billing` **must** return the zero policy for kinds the driver
does not bill-model. The conformance suite's `Billing/CapabilityContract`
subtest checks exactly this contract: non-negative fields, deterministic
answers, and the zero policy for undeclared kinds. Billing facts flow one
way — the driver states them, the kernel's lifecycle policy decides what to
do with them ([design doc 11 §20](https://github.com/samishal1998/fleetplane/blob/main/docs/11_COST_AWARE_LEASING.md)) — and
operators can override or disable them per instance and kind
([configuration → billing](/fleetplane/guides/configuration/#billing-providersnamebilling)).

## Optional capability: parking

A driver whose cloud bills stopped machines at a fraction of the running
price can expose stop/resume — the input to
[parked machines](/fleetplane/guides/concepts/#parked-machines-the-warm-tier)
([design doc 12](https://github.com/samishal1998/fleetplane/blob/main/docs/12_PARKED_MACHINES.md)) — by implementing the optional
`provider.ParkAware` interface
([`pkg/sdk/provider/parking.go`](https://github.com/samishal1998/fleetplane/blob/main/pkg/sdk/provider/parking.go)),
discovered by type assertion like `BillingAware`:

```go
type ParkAware interface {
    Parking(kind ResourceKind) ParkPolicy
}

type ParkPolicy struct {
    Supported     bool
    StartEstimate time.Duration // typical stopped->running latency hint
}
```

The zero `ParkPolicy` means "cannot park" — a provider that cannot park
never sees a stop action (the capability gate lives inside the journaling
transaction, not the caller) — and `Parking` **must** return the zero
policy for kinds the driver does not serve. A driver that declares support
also accepts two new `Action.Kind` values, `"stop"` and `"start"`, under a
hard contract:

- **Both are idempotent.** Stopping a stopped machine and starting a
  running machine return success — in *every* state combination. This is
  what makes crash recovery trivial: stop/start ops retry by plain
  re-dispatch, and a crash-duplicated dispatch is a harmless no-op.
- **Stop preserves identity.** Labels/tags, the external ID, and disks
  survive the cycle; ownership parsing (`FleetplaneID`, `CreateOpID`,
  `Owned`) must round-trip unchanged.
- **Report honest phases** while transitioning (`stopping` → `stopped`,
  `starting` → `running`), and surface hard rejections (a machine type that
  cannot stop) as `invalid` — the kernel reverts the phase and falls back to
  delete/create for that machine.

Two conformance subtests enforce this: `Parking/CapabilityContract` (stable,
non-negative policy; zero policy for undeclared kinds) and
`Parking/StopStartLifecycle` (full stop → start round trip, stop-of-stopped
and start-of-running succeed, identity survives).

## The ActionID dedup rule

`ActionID` is the operation ID: one ULID that is simultaneously the journal key
and the value of the `fleetplane.io/op` label. **A driver MUST make create
idempotent on the op label**: before creating, look for a resource that this
exact operation already produced, and return it instead of creating a second
one. Every in-tree driver opens `applyCreate` the same way:

```go
// A replayed create returns the resource the SAME operation already made.
if existing, err := d.Discover(ctx, provider.DiscoverRequest{
    Scope:    provider.ScopeOwned,
    Selector: map[string]string{provider.LabelOp: action.ActionID},
}); err == nil && len(existing) > 0 {
    return provider.OperationRef{ActionID: action.ActionID, Ref: &existing[0].Ref}, nil
}
```

This is the crash-safety anchor: if the process dies after the create request
was sent but before the response was persisted, the engine replays the same
action from the journal — and the op label guarantees the replay finds the
existing resource instead of leaking a duplicate
([ADR-013](/fleetplane/developers/adr/adr-013-registration-labels/),
[ADR-014](/fleetplane/developers/adr/adr-014-retries/)). A resource-ID label cannot serve this
role, because it does not distinguish retry attempts.

Two companions to the rule:

- **Delete of already-deleted is success**, never an error loop — the desired
  outcome already holds.
- During `ObserveOperation`, surface `not_found` as the typed error and let the
  engine interpret it by operation kind (for a delete it means success; for a
  create it triggers verification).

## Error classification

Every error a driver returns should be a `*provider.Error` with a `Class`
(how the engine schedules around it) and a `SideEffect` (whether re-executing
the mutation is safe):

| Class | Use for | Typical `SideEffect` |
|---|---|---|
| `not_found` | 404s; the referenced resource does not exist | `none` |
| `rate_limited` | 429s; set `RetryAfter` when known | `none` |
| `conflict` | Locked/conflicting state, uniqueness violations | `maybe` on mutations |
| `quota` | Account/project resource limits | `none` |
| `invalid` | Bad spec, bad credentials, selector matching nothing — fail fast, never retried blindly | `none` |
| `retryable` | 5xx, transport failures, anything transient or unknown | `maybe` on mutations |
| `terminal` | The provider reports a permanent failure | — |

`SideEffect` is the crash-consistency signal: `none` means the request provably
never reached the provider (a validation failure, a refused connection before
send), so a blind retry is safe; `maybe` means the outcome is uncertain and the
engine must run its resolution procedure (op-label discovery within the verify
window) before re-dispatching. The helpers default conservatively: an unknown
error classifies as `retryable`, and anything not explicitly marked
`EffectNone` is treated as `maybe`. When in doubt on a mutation path, say
`maybe` — a false `none` can duplicate infrastructure.

`Message` must be safe to log: never embed credentials or request bodies.
Include the provider's native error `Code` and `RequestID` when available.

## Reserved labels

The SDK exports the only label keys any Fleetplane component may use for
ownership and identity ([ADR-013](/fleetplane/developers/adr/adr-013-registration-labels/)):

```go
provider.LabelManaged // "fleetplane.io/managed" = "true"
provider.LabelOwner   // "fleetplane.io/owner"   = control-plane OwnerID
provider.LabelID      // "fleetplane.io/id"      = res_... resource ID
provider.LabelOp      // "fleetplane.io/op"      = op_... create-dedup anchor
provider.LabelClass   // "fleetplane.io/class"   = class the resource came from
provider.LabelTest    // "fleetplane.io/test"    — E2E only (ADR-015)
provider.LabelTestRun // "fleetplane.io/test-run"
```

The kernel composes them into `DesiredState.Labels`; the driver applies them
verbatim and parses them back in `Discover`/`Get` results (`FleetplaneID`,
`CreateOpID`, `Owned`). If the cloud has no native labels, encode them — the
DigitalOcean tag codec above is the template. Label-selector `Discover` is
mandatory for v1 drivers; a driver that truly cannot support it declares
`SupportsLabelDiscovery: false` in its `Descriptor`, and its uncertain
operations freeze for manual `:resolve` instead of auto-resolving
([ADR-017](/fleetplane/developers/adr/adr-017-operation-states/)).

## Registration

Register the factory from `init()`:

```go
const Driver = "mycloud"

func init() {
    provider.Register(Driver, func(ctx context.Context, cfg provider.InstanceConfig) (provider.Provider, error) {
        return New(ctx, cfg)
    })
}
```

Then add exactly one blank import to
[`cmd/fleetplane/modules.go`](https://github.com/samishal1998/fleetplane/blob/main/cmd/fleetplane/modules.go)
— the only file touched to add or remove a provider from a distribution
([ADR-013](/fleetplane/developers/adr/adr-013-registration-labels/)); there is no central switch
statement:

```go
import (
    _ "github.com/samishal1998/fleetplane/providers/digitalocean"
    _ "github.com/samishal1998/fleetplane/providers/fake"
    _ "github.com/samishal1998/fleetplane/providers/hetzner"
    _ "github.com/you/fleetplane-provider-mycloud" // out-of-tree works too
)
```

`Register` panics on a duplicate driver name (a build mistake, caught
immediately); referencing an unregistered driver in config fails boot with the
list of compiled drivers.

In the factory, `InstanceConfig` gives you the instance name, the control
plane's `OwnerID` (for ownership labels), the raw `settings` block to
unmarshal, a `secretref.Resolver`, and a logger. Resolve `secret://` references
**at construction time and nowhere else** — the resolved value must never be
persisted or logged:

```go
token := s.Token
if secretref.IsRef(token) {
    sec, err := cfg.Secrets.Resolve(ctx, token)
    if err != nil { /* return an invalid *provider.Error */ }
    token = string(sec.Reveal())
}
```

One more rule, easy to miss: **disable your cloud SDK's built-in retries.** The
operation engine is the only retry authority; a hidden HTTP-level retry of a
create bypasses journal accounting ([ADR-014](/fleetplane/developers/adr/adr-014-retries/)).

## The conformance suite

[`pkg/sdk/conformance`](https://github.com/samishal1998/fleetplane/tree/main/pkg/sdk/conformance)
is the acceptance bar: the same suite Fleetplane runs against its fake and
(env-gated) against real clouds. A driver that passes it upholds every contract
above. Wire it up as an ordinary Go test:

```go
func TestMyCloudConformance(t *testing.T) {
    p := mycloud.New(...) // your provider, pointed at a test endpoint or project
    conformance.Run(t, conformance.Harness{
        Provider: p,
        Kind:     "compute.machine",
        NewSpec: func(i int) json.RawMessage { // a valid, distinct spec per index
            return json.RawMessage(`{"serverType":"small","image":"slug:base"}`)
        },
        InvalidSpec: json.RawMessage(`{"serverType":""}`), // must classify invalid
        OwnerID:     "owner-test",
        Eventual:    true,  // provider may lag list-after-create
        Expensive:   false, // enables bulk pagination subtests; keep off vs real clouds
        MaxSteps:    200,   // bound on every polling loop (default 200)
    })
}
```

The subtest names are the contract:

| Subtest | Proves |
|---|---|
| `Descriptor/DeclaresKind` | The descriptor lists the kind under test |
| `Errors/GetMissingIsErrNotFound` | `Get` of a missing ref is a typed `not_found` |
| `Errors/InvalidSpecIsErrInvalid` | Bad specs classify `invalid` with `EffectNone` |
| `Lifecycle/CreateObserveGetDelete` | Full create → poll → get → delete round trip |
| `Lifecycle/DeleteOfDeleted` | Second delete is success, never an error loop |
| `Idempotency/OpLabelDedup` | Replayed `Apply` with the same `ActionID` returns the same resource |
| `EventualConsistency/GetBeforeList` | `Get` by ref is authoritative while lists lag |
| `Labels/OwnershipApplied` | All four identity labels land and parse back |
| `Discovery/Stability` | Consecutive lists never lose a resource |
| `Discovery/OwnedScopeOnlyOwned` | `ScopeOwned` returns only owned resources |
| `Operations/PollingReachesTerminal` | `ObserveOperation` reaches a terminal state |
| `Pagination/OverOnePage` | Multi-page discovery loses nothing (`Expensive` only) |
| `Billing/CapabilityContract` | Optional `BillingAware`: fields non-negative, answers deterministic, undeclared kinds fine-grained (skipped when not implemented) |
| `Parking/CapabilityContract` | Optional `ParkAware`: stable policy, non-negative `StartEstimate`, zero policy for undeclared kinds (skipped when not implemented) |
| `Parking/StopStartLifecycle` | Stop → start round trip; stop-of-stopped and start-of-running succeed; identity survives the cycle (skipped when parking unsupported) |

The fake provider passes the whole catalog under its most hostile deterministic
settings — multi-step async creates and deletes, list lag, forced pagination —
see [`providers/fake/conformance_test.go`](https://github.com/samishal1998/fleetplane/blob/main/providers/fake/conformance_test.go).
The Hetzner E2E test runs the identical suite against the real cloud.

## Checklist

- [ ] `Plan` is pure and its `Action`s JSON round-trip exactly.
- [ ] `Apply` returns the external ref at accept time.
- [ ] Create is idempotent on the `fleetplane.io/op` label.
- [ ] Delete of already-deleted succeeds.
- [ ] `Get` of a missing resource is `*Error{Class: ErrNotFound}`.
- [ ] `ObserveOperation` polls by reference and tolerates `Data == nil`.
- [ ] Every error is a `*provider.Error` with honest `Class` and `SideEffect`.
- [ ] Identity labels applied verbatim; `Discover` honors `ScopeOwned` and selectors.
- [ ] `Extensions` carries the full native object.
- [ ] Cloud SDK retries disabled.
- [ ] Registered via `init()` + blank import in `cmd/fleetplane/modules.go`.
- [ ] The conformance suite passes.

The design rationale is in the [provider SDK design doc](https://github.com/samishal1998/fleetplane/blob/main/docs/03_PROVIDER_SDK.md).
