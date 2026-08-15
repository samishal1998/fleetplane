# Providers

A **provider** is Fleetplane's connection to one external infrastructure authority —
Hetzner Cloud, DigitalOcean, AWS, GCP, or anything else that can create and destroy
resources. A **driver** is the code (`hetzner`, `digitalocean`, `aws`, `gcp`,
`fake`); a **provider instance**
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

Five drivers ship in the default binary (see
[`cmd/fleetplane/modules.go`](https://github.com/samishal1998/fleetplane/blob/main/cmd/fleetplane/modules.go)):

| Driver | Kinds | Notes |
|---|---|---|
| `hetzner` | `compute.machine` | Hetzner Cloud servers via hcloud-go v2 |
| `digitalocean` | `compute.machine` | DigitalOcean droplets via godo |
| `aws` | `compute.machine` | Amazon EC2 instances via aws-sdk-go-v2 |
| `gcp` | `compute.machine` | Google Compute Engine instances via compute/v1 |
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

### Billing

The driver declares Hetzner's per-started-hour billing — an hourly increment
plus a `5m` termination buffer — for `compute.machine`
([cost-aware leasing](concepts.md#cost-aware-leasing-billing-windows)). As a
result, when a class sets `reclaim.idleAfter`, an idle machine is not deleted
the moment the idle window elapses: the delete waits for the safe window just
before the next billing boundary, since the hour is already paid for. To get
plain idle-reclaim timing back, opt the kind out with `disabled: true` under
`providers.<name>.billing`
([configuration → billing](configuration.md#billing-providersnamebilling)).

No [parking](concepts.md#parked-machines-the-warm-tier): Hetzner bills
powered-off servers at full price, so the driver declares no park capability
and delete-and-recreate stays the optimal reclaim path.

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

### Billing

The driver declares hourly billing with a `5m` termination buffer for
`compute.machine` only (droplets bill per started hour; the monthly cap is
deliberately not modeled — hourly is the conservative model for window
scheduling). With `reclaim.idleAfter` on a class, idle droplets are therefore
deleted in the safe window before their next billing boundary rather than
immediately when the idle window elapses; opt out per kind with
`disabled: true` under `providers.<name>.billing`
([configuration → billing](configuration.md#billing-providersnamebilling)).

No [parking](concepts.md#parked-machines-the-warm-tier): DigitalOcean bills
powered-off droplets at full price, so the driver declares no park capability
and delete-and-recreate stays the optimal reclaim path.

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

## AWS

### Settings

```yaml
providers:
  aws-main:
    driver: aws
    settings:
      region: eu-central-1                              # required
      accessKeyId: secret://env/AWS_ACCESS_KEY_ID       # optional pair; omit both
      secretAccessKey: secret://env/AWS_SECRET_ACCESS_KEY   # for the ambient chain
```

| Key | Required | Default | Meaning |
|---|---|---|---|
| `region` | yes | — | AWS region (e.g. `eu-central-1`) |
| `accessKeyId` | no | ambient chain | Access key ID as a `secret://` reference; all-or-nothing with `secretAccessKey` |
| `secretAccessKey` | no | ambient chain | Secret access key as a `secret://` reference |
| `sessionToken` | no | none | Session token for temporary credentials; requires the static pair |
| `endpoint` | no | public AWS API | API base URL override (used by tests) |
| `subnetId` | no | account default | Subnet for created instances |
| `securityGroupIds` | no | account default | Security group IDs for created instances |
| `keyName` | no | none | EC2 key pair name for created instances |
| `instanceProfile` | no | none | IAM instance profile **name** attached to created instances (needs `iam:PassRole`) |
| `rps` | no | `5` | Sustained request rate toward the EC2 API |
| `burst` | no | `10` | Token-bucket burst size |
| `maxConcurrent` | no | `5` | Max in-flight API calls |

Setting only one of `accessKeyId`/`secretAccessKey` is a boot error
(`aws: accessKeyId and secretAccessKey are all-or-nothing`), as is a
`sessionToken` without the pair. When the pair is **omitted entirely**, the
driver uses the SDK's ambient credential chain — environment variables, shared
config/credentials files, IMDS/IRSA instance roles — so a Fleetplane host
running on AWS needs no keys in its config at all. The minimal IAM policy for
either route is in the
[setup guide → provider credentials and permissions](../../SETUP_GUIDE.md#12-provider-credentials-and-permissions).
SDK retries are capped at one attempt — the operation engine is the only retry
authority ([ADR-014](../adr/ADR-014-retries.md)) — and the shared pacer shapes
request rate (`rps`/`burst`/`maxConcurrent`).

### Instance types and images

```yaml
classes:
  ci-aws:
    kind: compute.machine
    provider: aws-main
    spec:
      serverType: t3.medium
      image: "snapshot:ci-runner=v12"
      location: eu-central-1a        # availability zone (optional)
```

`serverType` is an EC2 instance type name, passed through as-is. Reported
capacity comes from `DescribeInstanceTypes` (cached per type): `cpu` = default
vCPUs, `memoryMiB` = memory in MiB. `spec.location`, when set, is the
availability zone.

`image` takes one of three forms:

| Form | Example | Resolution |
|---|---|---|
| `id:<ami>` | `id:ami-0abc1234567890def` | Exact AMI ID |
| `name:<pattern>` | `name:ubuntu/images/hvm-ssd-gp3/*24.04*` | Available self- and Amazon-owned AMIs by name pattern; **newest wins** |
| `snapshot:<k=v>` | `snapshot:ci-runner=v12` | Available self-owned AMIs by tag equality; **newest wins** |

`snapshot:` is the image-pipeline form: tag the AMIs you build
(`ci-runner=v12`) and select by tag; the most recently created match is used.
A selector that matches nothing is a configuration error: the create fails
fast with an `invalid` error (`no image matches ...`) instead of retrying.

### Billing

The driver declares EC2's per-second billing with a **60-second minimum** for
`compute.machine` — a minimum duration, no increment
([cost-aware leasing](concepts.md#cost-aware-leasing-billing-windows)). With
no increment there are no billing-boundary windows to schedule deletes
around; the minimum only means a machine deleted within its first minute was
billed for the full minute. Override or disable per kind via
`providers.<name>.billing`
([configuration → billing](configuration.md#billing-providersnamebilling)).

### Parking

The driver implements the optional `ParkAware` capability for
`compute.machine` ([concepts → parked machines](concepts.md#parked-machines-the-warm-tier)):
EBS-backed instances stop and resume, and a stopped instance bills only its
EBS volumes and Elastic IPs. The declared `StartEstimate` is 30s — EC2
starts typically land in tens of seconds.

Two caveats the driver surfaces rather than hides:

- **Spot and instance-store-backed instances cannot stop.** EC2 rejects the
  call with `UnsupportedOperation`, which the driver maps to an `invalid`
  error — the kernel cleanly reverts `parking → ready` and falls back to
  delete/create semantics for that machine.
- **The public IP changes.** EC2 releases the ephemeral public IPv4 at stop
  and usually assigns a new one at start (only Elastic IPs are stable), so
  Fleetplane re-observes addresses and re-runs the readiness probe after
  every start — never reuse pre-stop addresses.

### Identity labels

AWS tag keys permit dots and slashes, so the `fleetplane.io/*` identity
labels ride as **native EC2 instance tags, verbatim — no codec**
([ADR-013](../adr/ADR-013-registration-labels.md)). Extra labels from
`spec.labels` are merged in (plus a `Name` tag from the desired name); on a
key collision the reserved labels win. Discovery filters server-side on the
ownership tags; create-dedup finds the op tag, and the op ID additionally
rides as EC2's native `ClientToken` idempotency token. Don't remove
`fleetplane.io/*` tags — ownership tracking, discovery, and crash recovery
depend on them.

### Instance state mapping

| EC2 state | Fleetplane observed phase |
|---|---|
| `pending` | `pending` |
| `running` | `running` |
| `stopping`, `stopped` | `stopped` |
| `shutting-down` | `deleting` |
| `terminated` | `gone` |
| anything else | `unknown` |

Terminated instances stay visible on EC2 for up to an hour; `Discover` skips
them (they are artifacts, not resources) while `Get` reports them as `gone`.
The full native instance object is preserved in `status.extensions`.

## GCP

### Settings

```yaml
providers:
  gcp-main:
    driver: gcp
    settings:
      project: my-project                                  # required
      zone: europe-west3-a                                 # required default zone
      credentialsJson: secret://file/etc/fleetplane/gcp-sa.json   # omit for ADC
```

| Key | Required | Default | Meaning |
|---|---|---|---|
| `project` | yes | — | GCP project ID |
| `zone` | yes | — | Default zone for instances; `spec.location` wins |
| `credentialsJson` | no | ADC | Service-account JSON key as a `secret://` reference; omitted = Application Default Credentials |
| `endpoint` | no | public GCE API | API base URL override (used by tests; disables authentication) |
| `network` | no | `global/networks/default` | Network for created instances |
| `subnetwork` | no | none | Subnetwork for created instances |
| `rps` | no | `5` | Sustained request rate toward the GCE API |
| `burst` | no | `10` | Token-bucket burst size |
| `maxConcurrent` | no | `5` | Max in-flight API calls |

Unlike the token drivers, `credentialsJson` **must** be a `secret://`
reference when set — a literal value is a boot error
(`gcp: credentialsJson must be a secret:// reference`). `secret://file/...`
is the natural fit for a JSON key. Omitting it entirely selects the ambient
Application Default Credentials chain (`GOOGLE_APPLICATION_CREDENTIALS`,
gcloud user credentials, metadata server), so a Fleetplane host running on
GCP needs no key in its config at all. Service-account permissions — the
simple `roles/compute.instanceAdmin.v1` path and the least-privilege list —
are in the
[setup guide → provider credentials and permissions](../../SETUP_GUIDE.md#12-provider-credentials-and-permissions).
The REST client performs no automatic retries (the operation engine owns
them, [ADR-014](../adr/ADR-014-retries.md)), and the shared pacer shapes
request rate.

### Machine types and images

```yaml
classes:
  ci-gcp:
    kind: compute.machine
    provider: gcp-main
    spec:
      serverType: e2-medium
      image: "family:debian-cloud/debian-12"
      location: europe-west3-b       # zone (optional; default is settings.zone)
```

`serverType` is a GCE machine type name. Reported capacity comes from
`machineTypes.get` (cached per zone/type): `cpu` = guest CPUs, `memoryMiB` =
memory in MiB. The instance zone is `spec.location` when set, otherwise
`settings.zone`; discovery is **project-wide** (aggregated across all zones),
so instances outside the default zone are still swept.

`image` takes one of four forms:

| Form | Example | Resolution |
|---|---|---|
| `id:<image>` | `id:my-image` | Image in your project by name; a self-link or partial URL passes through |
| `family:<[project/]family>` | `family:debian-cloud/debian-12` | **Latest image in the family**; bare family name = your project |
| `name:<[project/]name>` | `name:debian-cloud/debian-12-bookworm-v20240101` | Exact image by name |
| `snapshot:<k=v>` | `snapshot:ci-runner=v12` | Labeled image in your project; **newest wins** |

`snapshot:` is the image-pipeline form: label the images you build and select
by label; the most recently created match is used. A selector that matches
nothing is a configuration error: the create fails fast with an `invalid`
error (`no image matches ...`) instead of retrying.

### Billing

The driver declares GCE's per-second billing with a **60-second minimum** for
`compute.machine` — a minimum duration, no increment, exactly like the AWS
driver. No billing-boundary window scheduling results; override or disable
per kind via `providers.<name>.billing`
([configuration → billing](configuration.md#billing-providersnamebilling)).

### Parking

The driver implements the optional `ParkAware` capability for
`compute.machine` ([concepts → parked machines](concepts.md#parked-machines-the-warm-tier)):
GCE instances stop and resume, and a `TERMINATED` (stopped) instance incurs
no compute charge — only its disks and static IPs keep billing, so the warm
tier is economically real here. The declared `StartEstimate` is 45s, the
typical GCE stopped→running latency.

Two caveats the driver surfaces rather than hides:

- **The public IP changes.** GCE releases the ephemeral external IP at stop,
  so a restarted instance usually has a **new address** — Fleetplane
  re-reads addresses and re-runs the readiness probe after every start.
- **Local SSDs block a plain stop.** GCE rejects stopping an instance with a
  local SSD (without `discardLocalSsd`) with a 400; the driver maps it to an
  `invalid` error, the machine is untouched, and the kernel cleanly reverts
  `parking → ready` — delete/create semantics apply to that machine instead.

### Identity labels become `fp-*` labels

GCE labels are strictly lowercase `[a-z0-9_-]` (keys must start with a
letter, 63 chars max) — dots, slashes, and uppercase are all illegal, which
rules out `fleetplane.io/*` keys and uppercase ULID values. The driver
therefore encodes the reserved identity labels through a codec
([`providers/gcp/labels.go`](https://github.com/samishal1998/fleetplane/blob/main/providers/gcp/labels.go)) —
the GCP twin of DigitalOcean's tag codec. On an instance you will see:

| Label | GCE label on the instance |
|---|---|
| `fleetplane.io/managed=true` | `fp-managed=true` |
| `fleetplane.io/owner=<own_...>` | `fp-owner=<own_...>` (lowercased) |
| `fleetplane.io/id=<res_...>` | `fp-id=<res_...>` (lowercased) |
| `fleetplane.io/op=<op_...>` | `fp-op=<op_...>` (lowercased) |
| `fleetplane.io/class=<name>` | `fp-class=<name>` |
| `fleetplane.io/test`, `fleetplane.io/test-run` | `fp-test=<v>`, `fp-test-run=<v>` |

Values are lowercased on encode (GCE requires it); on decode the
ULID-carrying values (`fp-owner`, `fp-id`, `fp-op`, `fp-test-run`) regain
their canonical uppercase payload, so the SDK's label contract round-trips
unchanged. Unlike DigitalOcean, custom label keys from `spec.labels` are
**not dropped** — they are sanitized to the GCE charset (lowercased, illegal
runes become `-`, prefixed `u-` when not starting with a letter, truncated to
63 chars) and pass through decode verbatim. Discovery pushes the label
equalities down as a server-side filter and re-verifies them client-side
against the decoded labels.

### Instance status mapping

| GCE status | Fleetplane observed phase |
|---|---|
| `PENDING`, `PROVISIONING`, `STAGING` | `pending` |
| `RUNNING` | `running` |
| `PENDING_STOP`, `STOPPING`, `SUSPENDING`, `SUSPENDED`, `STOPPED`, `TERMINATED`, `DEPROVISIONING` | `stopped` |
| anything else (e.g. `REPAIRING`) | `unknown` |

Note GCE's `TERMINATED` means **stopped** — the instance still exists; a
deleted instance 404s instead (reported as the typed `not_found`). The full
native instance object is preserved in `status.extensions`.

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

### Optional capability: billing

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
do with them ([design doc 11 §20](../11_COST_AWARE_LEASING.md)) — and
operators can override or disable them per instance and kind
([configuration → billing](configuration.md#billing-providersnamebilling)).

### Optional capability: parking

A driver whose cloud bills stopped machines at a fraction of the running
price can expose stop/resume — the input to
[parked machines](concepts.md#parked-machines-the-warm-tier)
([design doc 12](../12_PARKED_MACHINES.md)) — by implementing the optional
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
| `Billing/CapabilityContract` | Optional `BillingAware`: fields non-negative, answers deterministic, undeclared kinds fine-grained (skipped when not implemented) |
| `Parking/CapabilityContract` | Optional `ParkAware`: stable policy, non-negative `StartEstimate`, zero policy for undeclared kinds (skipped when not implemented) |
| `Parking/StopStartLifecycle` | Stop → start round trip; stop-of-stopped and start-of-running succeed; identity survives the cycle (skipped when parking unsupported) |

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
