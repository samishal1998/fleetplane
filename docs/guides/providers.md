# Providers

A **provider** is Fleetplane's connection to one external infrastructure authority —
Hetzner Cloud, DigitalOcean, or anything else that can create and destroy resources.
A **driver** is the code (`hetzner`, `digitalocean`, `fake`); a **provider instance**
is one configured use of a driver, named by you in the config file. You can run
several instances of the same driver (say, two Hetzner projects) side by side.

```yaml
providers:
  hetzner-main:            # instance name — referenced by classes and pools
    driver: hetzner        # driver name — must be compiled into the binary
    settings:              # raw driver config; credentials are secret:// refs
      token: secret://env/HETZNER_TOKEN
      location: fsn1
```

Three drivers ship in the default binary (see
[`cmd/fleetplane/modules.go`](https://github.com/samishal1998/fleetplane/blob/main/cmd/fleetplane/modules.go)):

| Driver | Kinds | Notes |
|---|---|---|
| `hetzner` | `compute.machine` | Hetzner Cloud servers via hcloud-go v2 |
| `digitalocean` | `compute.machine` | DigitalOcean droplets via godo |
| `fake` | `compute.machine`, `storage.volume` | Deterministic in-memory provider for tests |

Check instance health with `fleetplane providers` or the Providers view of the
[web dashboard](dashboard.md) at `http://<server.addr>/ui/`. Health walks
`unknown → healthy → degraded → unavailable` (checked every 30s; degraded after 3
consecutive failures, unavailable after 10) and never affects `/health/ready` —
a cloud outage must not take the control plane out of rotation (design docs,
[07 §8](../07_SECURITY_AND_OPERATIONS.md)).

For the config file as a whole, see the [configuration guide](configuration.md).

## Hetzner

### Settings

```yaml
providers:
  hetzner-main:
    driver: hetzner
    settings:
      token: secret://env/HETZNER_TOKEN   # required
      location: fsn1                      # optional default location
```

| Key | Required | Default | Meaning |
|---|---|---|---|
| `token` | yes | — | API token as a `secret://` reference |
| `location` | no | none | Default server location (e.g. `fsn1`); `spec.location` wins |
| `endpoint` | no | public Hetzner API | API base URL override (used by tests) |
| `rps` | no | `5` | Sustained request rate toward the Hetzner API |
| `burst` | no | `10` | Token-bucket burst size |
| `maxConcurrent` | no | `5` | Max in-flight API calls |

The token must belong to a Hetzner Cloud project with read/write permission.
Credentials are always `secret://` references — the value is resolved once at boot
and never persisted or logged ([ADR-007](../adr/ADR-007-config.md)):

```yaml
token: secret://env/HETZNER_TOKEN          # from an environment variable
# or
token: secret://file/etc/fleetplane/hetzner-token   # from /etc/fleetplane/hetzner-token
```

`secret://file/<path>` reads an absolute path and trims one trailing newline. A
missing `token` fails boot with `hetzner: token is required (secret:// reference)`.

### Server types and images

A `compute.machine` spec for Hetzner:

```yaml
classes:
  ci-large:
    kind: compute.machine
    provider: hetzner-main
    spec:
      serverType: cpx31
      image: "snapshot:ci-runner=v12"
      location: fsn1
```

`serverType` is any Hetzner server type name (`cpx11` is the smallest x86 type).
It is resolved against the Hetzner API at create time; an unknown name fails
immediately with an `invalid` error. Reported capacity comes from the server
type: `cpu` = cores, `memoryMiB` = memory × 1024.

`image` takes one of three forms:

| Form | Example | Resolution |
|---|---|---|
| `id:<n>` | `id:12345` | Exact image ID |
| `name:<os>` | `name:ubuntu-24.04` | OS image by name, matched to the server type's architecture |
| `snapshot:<label-selector>` | `snapshot:ci-runner=v12` | Label selector over snapshots; **newest match wins** |

Hetzner snapshots have no names, so snapshots are selected by Hetzner label
selector — label your snapshots in your image pipeline (`ci-runner=v12`) and
reference them by that selector. When
several snapshots match, the most recently created one is used, so
`snapshot:ci-runner` (existence match) always picks the latest build. A selector
that matches nothing is a configuration error: the create fails fast with an
`invalid` error (`no image matches ...`) instead of retrying.

### Identity labels

Fleetplane stamps every server it creates with real Hetzner key=value labels,
applied verbatim ([ADR-013](../adr/ADR-013-registration-labels.md)):

```text
fleetplane.io/managed = true
fleetplane.io/owner   = <own_ULID>   # this control plane's identity
fleetplane.io/id      = <res_ULID>   # the Fleetplane resource ID
fleetplane.io/op      = <op_ULID>    # the create operation — the dedup anchor
```

Discovery lists by label selector
(`fleetplane.io/managed=true,fleetplane.io/owner=<own_...>`), so Fleetplane only
ever sees — and only ever deletes — servers it owns, even in a shared project.
Extra labels from `spec.labels` are merged in; on a key collision the reserved
labels win. Don't set `fleetplane.io/*` labels yourself outside of tests.

### Rate pacing

The driver paces itself against Hetzner's rate limit (3600 requests/hour,
refilling ~1/s) using the response headers, so a busy fleet degrades gracefully
instead of slamming into 429s:

- Base pacing: token bucket at `rps`/`burst` plus a `maxConcurrent` semaphore.
- `RateLimit-Remaining` < 20 → throttle to 1 request/s (the refill rate); the
  base rate is restored once remaining climbs back to 100.
- Remaining = 0 → park all calls until the reset time, capped at 60s.
- HTTP 429 → park for `20 − remaining` seconds (min 1s, max 60s).

While parked, driver calls return a `rate_limited` error with a `RetryAfter`
hint; the operation engine reschedules around it. hcloud-go's built-in retries
are explicitly disabled (`MaxRetries: 0`) — the operation engine is the only
retry authority, so the journal sees every attempt
([ADR-014](../adr/ADR-014-retries.md)).

### Server status mapping

| Hetzner status | Fleetplane observed phase |
|---|---|
| `initializing`, `starting`, `migrating`, `rebuilding` | `pending` |
| `running` | `running` |
| `off`, `stopping` | `stopped` |
| `deleting` | `deleting` |
| anything else | `unknown` |

The full native server object is preserved in `status.extensions` on the
resource, untouched.

### E2E testing against real Hetzner

The Hetzner end-to-end test (`tests/e2e_hetzner_test.go`) runs the full
conformance suite against the real API. It is double-gated and skips unless
**both** are set:

```bash
FLEETPLANE_E2E=1 HETZNER_TOKEN=... go test ./tests/ -run TestE2E -v
```

Safety rules ([ADR-015](../adr/ADR-015-e2e-safety.md)) — these are not optional:

- The token must belong to a **dedicated throwaway Hetzner project**. Never
  point E2E tests at a project containing anything you care about.
- Every E2E resource is labeled `fleetplane.io/test=1` and
  `fleetplane.io/test-run=<run id>`.
- Cleanup is layered: per-test `t.Cleanup`, an always-run CI sweeper step, and a
  nightly sweep. The sweeper (`scripts/e2esweep`) refuses to delete anything
  that does not carry `fleetplane.io/test=1` — conservative destruction applies
  to test tooling too.
- In CI, E2E runs only on main pushes, the nightly schedule, or manual dispatch
  — never on fork PRs.

## DigitalOcean

### Settings

```yaml
providers:
  do-main:
    driver: digitalocean
    settings:
      token: secret://env/DIGITALOCEAN_TOKEN   # required
      region: fra1                             # optional default region
```

| Key | Required | Default | Meaning |
|---|---|---|---|
| `token` | yes | — | API token as a `secret://` reference |
| `region` | no | none | Default droplet region (e.g. `fra1`); `spec.location` wins |
| `endpoint` | no | public DigitalOcean API | API base URL override (used by tests) |
| `rps` | no | `5` | Sustained request rate |
| `burst` | no | `10` | Token-bucket burst size |
| `maxConcurrent` | no | `5` | Max in-flight API calls |

The same rules as Hetzner apply: the token is a `secret://` reference, godo's
client performs no retries (the operation engine owns them,
[ADR-014](../adr/ADR-014-retries.md)), and the same header-driven pacer shapes
traffic around DigitalOcean's rate limit.

### Sizes, images, and region

```yaml
classes:
  do-runner:
    kind: compute.machine
    provider: do-main
    spec:
      serverType: s-2vcpu-4gb        # DigitalOcean size slug
      image: "snapshot:ci-runner"
```

`serverType` is a DigitalOcean size slug, passed through as the droplet size.
Reported capacity: `cpu` = vCPUs, `memoryMiB` = memory (DO reports MB, treated
as MiB). The droplet region is `spec.location` when set, otherwise
`settings.region`.

`image` takes one of three forms:

| Form | Example | Resolution |
|---|---|---|
| `id:<n>` | `id:12345` | Exact image ID |
| `slug:<distro-slug>` | `slug:ubuntu-24-04-x64` | Public distribution image by slug |
| `snapshot:<name>` | `snapshot:ci-runner` | Your snapshot **by name**; newest wins on duplicates |

Unlike Hetzner, DigitalOcean snapshots have names, so `snapshot:` takes a name,
not a label selector. If several of your snapshots share the name, the most
recently created one is used — so an image pipeline can keep uploading under the
same name. A name that matches nothing fails fast with an `invalid` error.

### Identity labels become `fp-*` tags

DigitalOcean has no key=value labels — only flat tags. The driver encodes
Fleetplane's identity labels as tags of the form `fp-<short>:<value>`
([`providers/digitalocean/tags.go`](https://github.com/samishal1998/fleetplane/blob/main/providers/digitalocean/tags.go));
the SDK contract (label maps) never changed, so the kernel is unaware of the
difference. On a droplet you will see:

| Label | Tag on the droplet |
|---|---|
| `fleetplane.io/managed=true` | `fp-managed:true` |
| `fleetplane.io/owner=<own_...>` | `fp-owner:<own_...>` |
| `fleetplane.io/id=<res_...>` | `fp-id:<res_...>` |
| `fleetplane.io/op=<op_...>` | `fp-op:<op_...>` |
| `fleetplane.io/class=<name>` | `fp-class:<name>` |
| `fleetplane.io/pool=<name>` | `fp-pool:<name>` |
| `fleetplane.io/test`, `fleetplane.io/test-run` | `fp-test:<v>`, `fp-test-run:<v>` |

Tag values are sanitized to DigitalOcean's tag charset (letters, digits, `:`,
`-`, `_`; anything else becomes `-`). ULID-based values pass through unchanged.
Only the reserved keys above have an encoding — unknown label keys are dropped,
because DO tags are not a general label store.

Because DigitalOcean lists by one tag at a time, discovery filters server-side
on the most selective tag available (`op` > `id` > `owner`) and applies the
remaining label equalities client-side. Behavior is identical to Hetzner's
label-selector discovery; only the mechanics differ.

### Droplet status mapping

| Droplet status | Fleetplane observed phase |
|---|---|
| `new` | `pending` |
| `active` | `running` |
| `off` | `stopped` |
| `archive` | `gone` |
| anything else | `unknown` |

## Writing a provider

A provider is a Go package implementing two interfaces from
[`pkg/sdk/provider`](https://github.com/samishal1998/fleetplane/tree/main/pkg/sdk/provider),
registered via `init()`, and proven by the conformance suite. The fake provider
([`providers/fake`](https://github.com/samishal1998/fleetplane/tree/main/providers/fake))
is the reference implementation — small, complete, and exercised by every kernel
test. Read it first; the design rationale is in the
[Provider SDK design doc](../03_PROVIDER_SDK.md).

### Boundary rules

Enforced mechanically by `make boundaries` ([ADR-009](../adr/ADR-009-boundaries.md)):

- Provider packages may import `pkg/sdk/...`, the kind packages (`pkg/kinds/...`),
  and their cloud SDK — **never** `internal/...`.
- The kernel (`internal/...`, `pkg/...`) never imports provider packages or
  cloud SDKs.
- `pkg/sdk` itself is stdlib-only, so out-of-tree providers depend on nothing
  but the SDK surface.

In-tree drivers also share the rate pacer in
[`providers/pacing`](https://github.com/samishal1998/fleetplane/tree/main/providers/pacing).

### The contract

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

### The ActionID dedup rule

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
([ADR-013](../adr/ADR-013-registration-labels.md),
[ADR-014](../adr/ADR-014-retries.md)). A resource-ID label cannot serve this
role, because it does not distinguish retry attempts.

Two companions to the rule:

- **Delete of already-deleted is success**, never an error loop — the desired
  outcome already holds.
- During `ObserveOperation`, surface `not_found` as the typed error and let the
  engine interpret it by operation kind (for a delete it means success; for a
  create it triggers verification).

### Error classification

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

### Reserved labels

The SDK exports the only label keys any Fleetplane component may use for
ownership and identity ([ADR-013](../adr/ADR-013-registration-labels.md)):

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
([ADR-017](../adr/ADR-017-operation-states.md)).

### Registration

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
([ADR-013](../adr/ADR-013-registration-labels.md)); there is no central switch
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
create bypasses journal accounting ([ADR-014](../adr/ADR-014-retries.md)).

### The conformance suite

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

The fake provider passes the whole catalog under its most hostile deterministic
settings — multi-step async creates and deletes, list lag, forced pagination —
see [`providers/fake/conformance_test.go`](https://github.com/samishal1998/fleetplane/blob/main/providers/fake/conformance_test.go).
The Hetzner E2E test runs the identical suite against the real cloud.

### Checklist

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
