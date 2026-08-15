# Configuration reference

Fleetplane reads a single YAML file, passed to the server at startup:

```bash
fleetplane serve --config /etc/fleetplane/config.yaml
```

Every key the binary understands is documented on this page, section by
section, with its type, default, and behavior. A complete annotated example
is at the [end of the page](#complete-annotated-example).

> **Strict YAML — unknown fields are boot errors.** The config file may only
> contain what the running binary actually implements
> ([ADR-007](../adr/ADR-007-config.md)). A typo like `serverr:` or a key from
> a newer version does not get silently ignored — `fleetplane serve` refuses
> to start and the error names the unknown field. Validation also runs up
> front: a class referencing an unknown provider, a malformed token record,
> or a missing `storage.path` all fail at boot, never at request time.

**Durations** are Go duration strings: `"20s"`, `"1m30s"`, `"15m"`. An
invalid duration (e.g. `shutdownGrace: soon`) is a boot error.

Several sections (`engine`, `reconcile`, `discovery`) have no config-file
defaults at all: leaving a key out (or zero) defers to the engine's built-in
defaults per [ADR-014](../adr/ADR-014-retries.md). The tables below note
these as "when zero". (`acquire.pendingTimeout` is the exception: it is
defaulted at config load — see [acquire](#acquire).)

## Top-level layout

```yaml
server:    {}   # listen addresses, shutdown
storage:   {}   # SQLite database path (required)
providers: {}   # provider instances (hetzner, digitalocean, fake)
classes:   {}   # reusable creation templates
engine:    {}   # operation engine tuning
reconcile: {}   # pool reconciler tuning
acquire:   {}   # acquisition handling
discovery: {}   # provider discovery sweep
auth:      {}   # static API tokens
```

## server

| Key | Type | Default | Behavior |
|---|---|---|---|
| `server.addr` | string | `":8080"` | Main API listen address. Serves `/v1/*`, `/health/*`, and the embedded [web dashboard](dashboard.md) at `/ui/` (`/` redirects there). |
| `server.opsAddr` | string | `"127.0.0.1:9090"` | Ops listener: `/metrics` (Prometheus), `/debug/pprof/*`, and `POST /admin/backup` (see the [backup runbook](../runbooks/backup-restore.md)). Keep it loopback-only unless the network is trusted. |
| `server.shutdownGrace` | duration | `20s` | Bound on graceful shutdown: readiness flips to false, in-flight HTTP drains within this window, then providers and the store close. In-flight provider operations resume from the journal on next boot. |

```yaml
server:
  addr: ":8080"
  opsAddr: "127.0.0.1:9090"
  shutdownGrace: 20s
```

The dashboard is served by `fleetplane serve` itself — embedded in the
binary, same listener as the API, no extra deployment. See the
[dashboard guide](dashboard.md).

## storage

| Key | Type | Default | Behavior |
|---|---|---|---|
| `storage.path` | string | **required** | SQLite database file path. Missing or empty is a boot error (`storage.path is required`). |

```yaml
storage:
  path: /var/lib/fleetplane/fleetplane.db
```

Fleetplane uses SQLite in WAL mode
([ADR-003](../adr/ADR-003-sqlite-driver.md)); hot backups run via
`fleetplane admin backup` against the ops listener.

## providers

`providers` is a map of **instance name** → provider configuration. The
instance name is yours to choose (`hetzner-main`, `do-fra1`, …); classes
reference it. Two instances of the same driver (e.g. two Hetzner projects)
are fine.

| Key | Type | Default | Behavior |
|---|---|---|---|
| `providers.<name>.driver` | string | **required** | One of `hetzner`, `digitalocean`, `fake`. A provider without a driver is a boot error. |
| `providers.<name>.settings` | map | — | Raw driver config block, decoded by the driver itself (tables below). |
| `providers.<name>.billing` | map | — | Per-kind billing-policy overrides for cost-aware leasing ([below](#billing-providersnamebilling)). |

Provider instances are constructed at boot: a bad settings block, a missing
credential, or an unresolvable `secret://` reference fails `fleetplane
serve` immediately with the provider name in the error.

### Credentials: `secret://` references

Credentials in `settings` must be `secret://` references
([ADR-007](../adr/ADR-007-config.md)); values are resolved at boot and never
logged or persisted. Two schemes exist:

```yaml
settings:
  token: secret://env/HETZNER_TOKEN      # value of the env var HETZNER_TOKEN
  # token: secret://file/etc/fleetplane/hetzner.token   # contents of /etc/fleetplane/hetzner.token
```

- `secret://env/NAME` — reads the environment variable `NAME` at boot. Unset
  variable → boot error.
- `secret://file/<path>` — reads the file at the **absolute** path `/<path>`
  (the path after `file/` is rooted at `/`). One trailing newline (and a
  preceding `\r`, if present) is trimmed, matching how secrets are commonly
  provisioned. Unreadable file → boot error.

What happens if a credential is **not** a secret reference: the drivers use
the literal string as the credential, as-is. It will authenticate — and your
API token now sits in plaintext in the config file. Don't do this outside
throwaway experiments; always use a `secret://` reference. An empty or
missing `token` is a boot error (`token is required (secret:// reference)`).

### Driver: `hetzner`

Drives `compute.machine` on Hetzner Cloud via the official hcloud-go v2 SDK.
SDK-level retries are disabled — the operation engine is the only retry
authority ([ADR-014](../adr/ADR-014-retries.md)).

| Setting | Type | Default | Behavior |
|---|---|---|---|
| `token` | string | **required** | Hetzner Cloud API token; use a `secret://` reference. |
| `location` | string | — | Default server location (e.g. `fsn1`). A class spec's `location` wins over this. |
| `endpoint` | string | — | API endpoint override (test use). |
| `rps` | float | `5` when zero | Request-rate limit toward the Hetzner API. |
| `burst` | int | `10` when zero | Rate-limiter burst. |
| `maxConcurrent` | int | `5` when zero | Max concurrent in-flight API calls. |

Image syntax accepted in `spec.image` for Hetzner classes:

- `id:<n>` — image by numeric ID
- `name:<os>` — OS image by name (e.g. `name:ubuntu-24.04`)
- `snapshot:<label-selector>` — snapshot by label selector (snapshots have
  no names on Hetzner); the newest match wins, a selector matching nothing
  fails fast as invalid rather than retrying.

```yaml
providers:
  hetzner-main:
    driver: hetzner
    settings:
      token: secret://env/HETZNER_TOKEN
      location: fsn1
```

### Driver: `digitalocean`

Drives `compute.machine` on DigitalOcean droplets via godo. Same retry
stance as Hetzner: the engine owns all retries.

| Setting | Type | Default | Behavior |
|---|---|---|---|
| `token` | string | **required** | DigitalOcean API token; use a `secret://` reference. |
| `region` | string | — | Default droplet region. A class spec's `location` wins over this. |
| `endpoint` | string | — | API endpoint override (test use). |
| `rps` | float | `5` when zero | Request-rate limit toward the DO API. |
| `burst` | int | `10` when zero | Rate-limiter burst. |
| `maxConcurrent` | int | `5` when zero | Max concurrent in-flight API calls. |

Image syntax accepted in `spec.image` for DigitalOcean classes:

- `id:<n>` — image by numeric ID
- `slug:<distro-slug>` — distribution image by slug (e.g. `slug:ubuntu-24-04-x64`)
- `snapshot:<name>` — snapshot by name (DO snapshots have names); the newest
  wins on duplicates.

DigitalOcean has flat tags rather than key=value labels, so the driver
encodes Fleetplane's reserved identity labels as `fp-<short>:<value>` tags;
unknown label keys from `spec.labels` are dropped on DO
([ADR-013](../adr/ADR-013-registration-labels.md)).

```yaml
providers:
  do-fra1:
    driver: digitalocean
    settings:
      token: secret://env/DO_TOKEN
      region: fra1
```

### Driver: `fake`

An in-memory provider supporting both `compute.machine` and
`storage.volume`, used by tests and conformance suites. No credentials. The
zero-value settings are fully synchronous (creates land running
immediately, lists see everything at once); the knobs introduce
deterministic asynchrony:

| Setting | Type | Default | Behavior |
|---|---|---|---|
| `createSteps` | int | `0` | Observe calls until a create lands `running`. |
| `deleteSteps` | int | `0` | Observe calls until a delete lands gone. |
| `listLagSteps` | int | `0` | Discover calls before a new object becomes listable. |
| `pageSize` | int | `0` | Internal Discover pagination chunk (`0` = single page). |

```yaml
providers:
  local:
    driver: fake
    settings: {}
```

### Billing (`providers.<name>.billing`)

Optional per-kind overrides of the driver's billing policy — the input to
cost-aware leasing ([design doc 11](../11_COST_AWARE_LEASING.md),
[ADR-018](../adr/ADR-018-cost-aware-leasing.md); the behavior it enables is
explained in [concepts → cost-aware leasing](concepts.md#cost-aware-leasing-billing-windows)).
`billing` is a map of **resource kind** → override; override kinds are
validated against the driver's declared kinds at boot, so a typo'd or
unserved kind is a boot error.

| Key | Type | Default | Behavior |
|---|---|---|---|
| `billing.<kind>.minimumDuration` | duration | driver's value | Shortest period the provider ever bills (`0` = none). Must be >= 0. |
| `billing.<kind>.increment` | duration | driver's value | Billing granularity after the minimum (`0` = fine-grained, per-use). Must be >= 0. |
| `billing.<kind>.terminationBuffer` | duration | driver's value | How long before a billing boundary a delete is dispatched so provider-side termination completes inside the paid window. Must be >= 0. |
| `billing.<kind>.adaptive` | bool | `false` | Derive the termination buffer from observed provider deletion durations (p95 + margin, floored by `terminationBuffer`, capped at half the increment). |
| `billing.<kind>.disabled` | bool | `false` | Explicit opt-out: the kind gets the zero (fine-grained) policy and no billing-window behavior. May not be combined with the other fields — that is a boot error. |

Overrides **merge field-wise** over the driver's declared policy: an unset
field keeps the driver's value, so a partial override never silently zeroes
the rest. The shipped driver defaults:

| Driver | Default billing policy |
|---|---|
| `hetzner` | Hourly increment + `5m` termination buffer for `compute.machine` |
| `digitalocean` | Hourly increment + `5m` termination buffer for `compute.machine` only |
| `fake` | Whatever its `settings.billing` block declares (zero policy by default) |

A kind with the zero policy — no driver declaration, no override — gets
fine-grained billing: ordinary lease and idle reclamation, no billing-window
scheduling. Cost-aware behavior is therefore **off by default** and per
provider instance, per kind.

```yaml
providers:
  hetzner-main:
    driver: hetzner
    settings:
      token: secret://env/HETZNER_TOKEN
    billing:
      compute.machine:
        terminationBuffer: 10m   # partial override — increment stays hourly
        adaptive: true           # learn the buffer from observed deletes
```

## classes

Classes are reusable creation templates (see the design docs,
[04 §2](../04_API_AND_RESOURCE_MODEL.md)). A resource created "from class
`ci-large`" expands the class spec; pools also target classes.

| Key | Type | Default | Behavior |
|---|---|---|---|
| `classes.<name>.kind` | string | **required** | Resource kind: `compute.machine` or `storage.volume`. |
| `classes.<name>.provider` | string | **required** | Name of a configured provider instance. Referencing an unknown provider is a boot error. |
| `classes.<name>.spec` | map | — | Kind-specific spec (below). Provider-specific extra fields are allowed and passed through to the driver. |
| `classes.<name>.reclaim.idleAfter` | duration | — | Poolless idle reclamation (below). Absent = never auto-reclaimed. |
| `classes.<name>.scheduling.queue.maxWait` | duration | — | Queue budget for acquisitions of this class (below). Absent/`0` = provision immediately. |

### `compute.machine` spec

| Field | Type | Required | Behavior |
|---|---|---|---|
| `serverType` | string | yes | Provider server type (e.g. `cpx31` on Hetzner, `s-2vcpu-4gb` on DO). |
| `image` | string | yes | Image reference; syntax is per-driver (see the driver sections above). |
| `location` | string | no | Placement; overrides the provider's default `location`/`region`. |
| `userData` | string | no | Cloud-init user data. |
| `labels` | map | no | Labels applied to the provider resource (verbatim on Hetzner; encoded as tags on DO). |
| `readiness` | object | no | Workload readiness probe (below). |

Capacity dimensions for scheduling: `cpu`, `memoryMiB`.

#### Readiness probes

The readiness probe is a post-create gate executed by the operation engine
after the provider reports success: the machine only becomes `ready` once
the probe passes. Declare **exactly one** of `tcp` or `http` — setting both,
or setting `readiness` with neither, is a validation error.

| Field | Type | Default | Behavior |
|---|---|---|---|
| `readiness.tcp.port` | int | required for tcp | Port to dial; must be 1–65535. Passes when the TCP connection succeeds. |
| `readiness.tcp.timeout` | duration | `3s` | Per-attempt dial timeout. |
| `readiness.http.port` | int | required for http | Port for `GET http://<addr>:<port><path>`; must be 1–65535. |
| `readiness.http.path` | string | required for http | Request path; must start with `/`. |
| `readiness.http.expectStatus` | int | `0` | Exact status code required; `0` means any 2xx passes. |
| `readiness.http.timeout` | duration | `3s` | Per-attempt request timeout. |
| `readiness.initialDelay` | duration | `0s` | Wait before the first attempt. |
| `readiness.period` | duration | `5s` | Interval between attempts. |
| `readiness.budget` | duration | `5m` | Total time before the probe declares failure. |
| `readiness.successThreshold` | int | `1` | Consecutive passes required. |

The probe targets the machine's first usable address, preferring public
IPv4, then public IPv6, then private.

```yaml
classes:
  web:
    kind: compute.machine
    provider: hetzner-main
    spec:
      serverType: cpx21
      image: "name:ubuntu-24.04"
      readiness:
        http:
          port: 8080
          path: /healthz
          expectStatus: 200
        period: 5s
        budget: 3m
```

### `storage.volume` spec

| Field | Type | Required | Behavior |
|---|---|---|---|
| `sizeGiB` | int | yes | Volume size; must be > 0. |
| `zone` | string | no | Placement zone. |
| `filesystem` | string | no | Provider-interpreted, opaque to the kernel. |

Capacity dimension: `storageGiB`.

```yaml
classes:
  scratch-100:
    kind: storage.volume
    provider: local
    spec:
      sizeGiB: 100
```

### `reclaim.idleAfter`

Poolless resources created from a class with a reclaim policy are deleted
automatically once idle: every reconcile cycle, a class-created resource
that is `ready`, has **no active leases**, is not delete-protected, and has
been idle for at least `idleAfter` is drained and deleted (a journaled
delete, counted against the reconciler's mutation budget). Pool-managed
resources are sized by the pool instead. A class without `reclaim` is never
auto-reclaimed.

```yaml
classes:
  ci-large:
    kind: compute.machine
    provider: hetzner-main
    spec:
      serverType: cpx31
      image: "snapshot:ci-runner=v12"
    reclaim:
      idleAfter: 5m
```

### `scheduling.queue.maxWait`

The class's cost/latency tradeoff for
[cost-aware leasing](concepts.md#cost-aware-leasing-billing-windows): with
`maxWait` set, acquisitions of this class **wait for existing or in-flight
capacity** for up to this long before scaling up (sequential lease packing).
At the deadline the scheduler force-scales — a queued acquisition never waits
past its budget. `0` or absent means provision immediately (the previous
behavior). A per-request `maxWait` (API/CLI) overrides the class default.

Validation: must be >= 0 and must not exceed `acquire.pendingTimeout` — a
larger value is a boot error. The effective per-acquisition value is
additionally clamped to `pendingTimeout - 10s` at accept time (see
[acquire](#acquire)).

```yaml
classes:
  ci-large:
    kind: compute.machine
    provider: hetzner-main
    spec:
      serverType: cpx31
      image: "snapshot:ci-runner=v12"
    scheduling:
      queue:
        maxWait: 10m    # cost-oriented: pack work into already-paid windows
```

## engine

Tunes the operation engine — the component that executes every provider
mutation exactly once via a persistent journal
([ADR-014](../adr/ADR-014-retries.md),
[ADR-017](../adr/ADR-017-operation-states.md)).

| Key | Type | Default | Behavior |
|---|---|---|---|
| `engine.pollInterval` | duration | `2s` when zero | Engine wake-up cadence. This is a fallback — in-process kicks are the primary trigger — so there is rarely a reason to change it. |
| `engine.verifyWindow` | duration | `120s` when zero | How long a create with an unknown outcome (crash mid-call, ambiguous provider answer) is probed before the operation freezes as `uncertain` for manual `:resolve`. |

```yaml
engine:
  verifyWindow: 120s
```

## reconcile

Tunes the pool reconciler.

| Key | Type | Default | Behavior |
|---|---|---|---|
| `reconcile.interval` | duration | `15s` when zero | Reconciler period. |
| `reconcile.maxMutationsPerCycle` | int | `5` when zero | The one budget knob: maximum provider mutations (creates, deletes, reclaims) per reconcile cycle, across pools. Caps blast radius when a pool spec is wrong. |

```yaml
reconcile:
  interval: 15s
  maxMutationsPerCycle: 5
```

## acquire

| Key | Type | Default | Behavior |
|---|---|---|---|
| `acquire.pendingTimeout` | duration | `15m` | Acquisitions that were never satisfied (no capacity, provider down) expire after this long instead of pending forever. Defaulted **at config load** (not deep in the engine), so the queue clamp below and the expiry sweep always see the same effective value. |

An acquisition's queue budget — the request's `maxWait`, else the class's
`scheduling.queue.maxWait` — is clamped to `pendingTimeout - 10s` at accept
time. Clamped, never rejected: rejecting against a config-dependent limit
would break byte-identical idempotent replays.

```yaml
acquire:
  pendingTimeout: 15m
```

## discovery

Tunes the provider discovery sweep — the loop that compares what the
provider actually has against what Fleetplane's records say, and resolves
ghosts and orphans ([ADR-017](../adr/ADR-017-operation-states.md)). The
sweep only runs against providers whose health is `healthy` or `unknown`;
orphan confirmation is suspended while a provider is unhealthy, because
absence from a listing is never proof.

| Key | Type | Default | Behavior |
|---|---|---|---|
| `discovery.interval` | duration | `30s` when zero | Sweep period. |
| `discovery.orphanGrace` | duration | `60s` when zero | Grace between confirming a local record's provider resource is gone (by direct read, not just absent from a list) and tombstoning the record as orphaned. |
| `discovery.ghostPolicy` | string | `delete` | What to do with a **ghost** — a provider resource carrying Fleetplane's labels but matching no live record. `delete`: reclaim it via a journaled delete. `surface`: record it, log a warning, and leave deletion to you. |
| `discovery.adoptUnlabeled` | string | `off` | What to do with provider resources carrying **no** Fleetplane labels. `off`: ignore them entirely (the sweep only lists owned resources). `observed`: list everything and record unlabeled resources with ownership `observed` — visible in the inventory, never mutated. |

```yaml
discovery:
  interval: 30s
  orphanGrace: 60s
  ghostPolicy: delete
  adoptUnlabeled: off
```

## auth

Static API tokens ([ADR-008](../adr/ADR-008-tokens.md)). With **no tokens
configured, the API runs open** — every request is allowed. That is for
local development only, and boot logs a loud warning. (The
[dashboard](dashboard.md) likewise needs no token when the API is open.)

| Key | Type | Default | Behavior |
|---|---|---|---|
| `auth.tokens` | list | empty (API open) | Static token records. |
| `auth.tokens[].id` | string | **required** | 8-hex lookup prefix. Duplicate IDs are a boot error. |
| `auth.tokens[].name` | string | — | Human name; shown in audit events and 403 messages. |
| `auth.tokens[].sha256` | string | **required** | Hex SHA-256 of the token secret; must be exactly 64 hex characters. The plaintext secret is never stored anywhere. |
| `auth.tokens[].permissions` | list | — | Permission names from the closed set below. An unknown permission name is a boot error. |

Clients present tokens as `Authorization: Bearer flp_<id8>.<secret>`.
Generate a token and its ready-to-paste config snippet locally (no server
call):

```bash
fleetplane token new --name ci --perm resource.read --perm resource.acquire --perm operation.read
```

The plaintext is printed once; the command also prints the `auth.tokens`
YAML entry to add to this file.

### Permissions

The closed set, verbatim from the code:

| Permission | Grants |
|---|---|
| `resource.read` | List and read resources; read acquisitions. |
| `resource.acquire` | Acquire and release resources. |
| `resource.create` | Create resources. |
| `resource.delete` | Delete and drain resources. |
| `pool.read` | List and read pools. |
| `pool.write` | Create, update, and reconcile pools. |
| `provider.read` | Read provider instance health. |
| `provider.admin` | Resolve `uncertain` operations (`:resolve`). |
| `operation.read` | List and read operations; read audit events. |
| `admin` | Everything above. |

A token holding `admin` passes every permission check. A token lacking a
required permission gets `403 permission_denied` ("token `<name>` lacks
`<perm>`").

```yaml
auth:
  tokens:
    - id: 8f3a1c2d
      name: ci
      sha256: "9d2f6c1a...<64 hex chars total>...b4e7"
      permissions: [resource.read, resource.acquire, operation.read]
    - id: 1a2b3c4d
      name: ops
      sha256: "77aa01fe...<64 hex chars total>...c9d2"
      permissions: [admin]
```

## Complete annotated example

Everything below is optional except `storage.path` (and, in practice, at
least one provider and one class if you want the server to do anything).
This mirrors [`examples/config.yaml`](../../examples/config.yaml), expanded
to show every section.

```yaml
# Fleetplane configuration.
# Strict YAML: any key not listed in this reference is a boot error (ADR-007).
# Durations are Go strings: "20s", "1m30s", "15m".

server:
  addr: ":8080"                # API + dashboard (/ui/); default ":8080"
  opsAddr: "127.0.0.1:9090"    # metrics/pprof/backup; keep loopback
  shutdownGrace: 20s           # graceful shutdown bound

storage:
  path: /var/lib/fleetplane/fleetplane.db   # REQUIRED; SQLite, WAL mode

# Provider instances. Credentials are ALWAYS secret:// references:
#   secret://env/NAME            -> env var NAME
#   secret://file/etc/fp/token   -> contents of /etc/fp/token (one trailing \n trimmed)
providers:
  hetzner-main:
    driver: hetzner
    settings:
      token: secret://env/HETZNER_TOKEN
      location: fsn1           # default location; spec.location wins
    # billing:                 # cost-aware leasing: per-kind overrides that
    #   compute.machine:       #   merge field-wise over the driver's defaults
    #     terminationBuffer: 10m
    #     adaptive: true       # learn the buffer from observed deletes
    #     # disabled: true     # or: opt the kind out entirely (alone, no other fields)
  do-fra1:
    driver: digitalocean
    settings:
      token: secret://env/DO_TOKEN
      region: fra1             # default region; spec.location wins

# Classes: reusable creation templates. spec is kind-specific and may carry
# provider-specific extras; reclaim.idleAfter auto-deletes idle poolless
# resources created from the class.
classes:
  ci-large:
    kind: compute.machine
    provider: hetzner-main
    spec:
      serverType: cpx31
      image: "snapshot:ci-runner=v12"   # hetzner: id:<n> | name:<os> | snapshot:<selector>
      location: fsn1
      readiness:                         # engine-run gate after provider success
        tcp:
          port: 22
        period: 5s                       # defaults: timeout 3s, period 5s,
        budget: 3m                       #   budget 5m, successThreshold 1
    reclaim:
      idleAfter: 5m                      # idle + no leases for 5m -> deleted
    # scheduling:
    #   queue:
    #     maxWait: 10m                   # queue for existing capacity before scaling up

engine:
  pollInterval: 2s             # fallback wake-up; kicks are primary
  verifyWindow: 120s           # uncertain-create probing window

reconcile:
  interval: 15s
  maxMutationsPerCycle: 5      # blast-radius cap per cycle

acquire:
  pendingTimeout: 15m          # never-satisfied acquisitions expire; also caps maxWait

discovery:
  interval: 30s
  orphanGrace: 60s
  ghostPolicy: delete          # delete | surface
  adoptUnlabeled: off          # off | observed

# Static API tokens. Empty list = OPEN API (dev only; loud boot warning).
# Generate entries with: fleetplane token new --name ci --perm resource.acquire ...
auth:
  tokens:
    - id: 8f3a1c2d
      name: ci
      sha256: "<64 hex chars: hex(sha256(secret))>"
      permissions: [resource.read, resource.acquire, operation.read]
```

## See also

- [Dashboard guide](dashboard.md) — the embedded web UI served at `/ui/`
- [Backup and restore runbook](../runbooks/backup-restore.md) — uses `server.opsAddr`
- Design docs: [security & operations](../07_SECURITY_AND_OPERATIONS.md)
  (tokens, secrets, ops listener),
  [API & resource model](../04_API_AND_RESOURCE_MODEL.md) (classes, kinds),
  [reconciliation & scheduling](../05_RECONCILIATION_AND_SCHEDULING.md)
  (pools, discovery),
  [cost-aware leasing](../11_COST_AWARE_LEASING.md) (billing windows, queueing)
- ADRs: [ADR-007 config & secrets](../adr/ADR-007-config.md),
  [ADR-008 tokens](../adr/ADR-008-tokens.md),
  [ADR-014 retries](../adr/ADR-014-retries.md),
  [ADR-017 operation states, ghosts & orphans](../adr/ADR-017-operation-states.md),
  [ADR-018 cost-aware leasing](../adr/ADR-018-cost-aware-leasing.md)
