---
title: "Hetzner Cloud"
description: "Run Fleetplane machines on Hetzner Cloud: token, image selectors, billing, identity labels, rate pacing and E2E testing."
---

This page covers the `hetzner` driver: a walkthrough from credentials to first machine, the permissions it needs, and the full settings reference. For how providers fit together, see the [providers overview](/fleetplane/guides/providers/overview/).

## Walkthrough

1. **Create a project and API token.** In the Hetzner Cloud console, use a dedicated project for Fleetplane, then create a Read/Write API token under Security → API tokens.

2. **Export the token** where the server runs (or put it in the systemd `EnvironmentFile`):

   ```bash
   export HETZNER_TOKEN=<token>
   ```

3. **Configure the provider and a class:**

   ```yaml
   providers:
     hetzner-main:
       driver: hetzner
       settings:
         token: secret://env/HETZNER_TOKEN
         location: fsn1          # default server location; spec.location wins

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

   Optional settings: `endpoint` (API override, mainly for tests), `rps`, `burst`, `maxConcurrent` (request pacing; defaults 5 / 10 / 5).

4. **Image selector syntax** — the `image` field takes one of three forms:

   | Form | Example | Selects |
   |---|---|---|
   | `id:<n>` | `id:123456` | An image by numeric ID |
   | `name:<os>` | `name:ubuntu-24.04` | A system image by name |
   | `snapshot:<label-selector>` | `snapshot:ci-runner=v12` | Newest snapshot matching the label selector |

   Hetzner snapshots have no names — label your snapshot (e.g. `ci-runner=v12`) when you create it, and select by label. When several match, the newest wins. A selector matching nothing is a fail-fast config error, not a retry.

5. **Try it:**

   ```bash
   fleetplane acquire --class ci-large --cpu 2 --ttl 90m
   fleetplane watch acq_01J...
   fleetplane resources
   ```

Fleetplane marks everything it creates with `fleetplane.io/*` labels (`managed`, `owner`, `id`, `op`, `class`) — leave them in place; discovery and crash recovery depend on them.

## Credentials and permissions

Create a **project-scoped API token** with **Read & Write** permission: in the Hetzner Cloud console, select (ideally) a dedicated project, then Security → API tokens → Generate API token. Hetzner tokens have no finer-grained scoping than read vs. read/write; write is required because Fleetplane creates and deletes servers. Using a dedicated project is the blast-radius limit: the token can only touch that project's resources.

```yaml
settings:
  token: secret://env/HETZNER_TOKEN
```

## Settings

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
and never persisted or logged ([ADR-007](/fleetplane/developers/adr/adr-007-config/)):

```yaml
token: secret://env/HETZNER_TOKEN          # from an environment variable
# or
token: secret://file/etc/fleetplane/hetzner-token   # from /etc/fleetplane/hetzner-token
```

`secret://file/<path>` reads an absolute path and trims one trailing newline. A
missing `token` fails boot with `hetzner: token is required (secret:// reference)`.

## Server types and images

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

## Billing

The driver declares Hetzner's per-started-hour billing — an hourly increment
plus a `5m` termination buffer — for `compute.machine`
([cost-aware leasing](/fleetplane/guides/concepts/#cost-aware-leasing-billing-windows)). As a
result, when a class sets `reclaim.idleAfter`, an idle machine is not deleted
the moment the idle window elapses: the delete waits for the safe window just
before the next billing boundary, since the hour is already paid for. To get
plain idle-reclaim timing back, opt the kind out with `disabled: true` under
`providers.<name>.billing`
([configuration → billing](/fleetplane/guides/configuration/#billing-providersnamebilling)).

No [parking](/fleetplane/guides/concepts/#parked-machines-the-warm-tier): Hetzner bills
powered-off servers at full price, so the driver declares no park capability
and delete-and-recreate stays the optimal reclaim path.

## Identity labels

Fleetplane stamps every server it creates with real Hetzner key=value labels,
applied verbatim ([ADR-013](/fleetplane/developers/adr/adr-013-registration-labels/)):

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

## Rate pacing

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
([ADR-014](/fleetplane/developers/adr/adr-014-retries/)).

## Server status mapping

| Hetzner status | Fleetplane observed phase |
|---|---|
| `initializing`, `starting`, `migrating`, `rebuilding` | `pending` |
| `running` | `running` |
| `off`, `stopping` | `stopped` |
| `deleting` | `deleting` |
| anything else | `unknown` |

The full native server object is preserved in `status.extensions` on the
resource, untouched.

## E2E testing against real Hetzner

The Hetzner end-to-end test (`tests/e2e_hetzner_test.go`) runs the full
conformance suite against the real API. It is double-gated and skips unless
**both** are set:

```bash
FLEETPLANE_E2E=1 HETZNER_TOKEN=... go test ./tests/ -run TestE2E -v
```

Safety rules ([ADR-015](/fleetplane/developers/adr/adr-015-e2e-safety/)) — these are not optional:

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
