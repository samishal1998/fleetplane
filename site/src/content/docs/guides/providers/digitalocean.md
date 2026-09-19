---
title: "DigitalOcean"
description: "Run Fleetplane machines as DigitalOcean droplets: token, image selectors, billing and fp-* identity tags."
---

This page covers the `digitalocean` driver: a walkthrough from credentials to first machine, the permissions it needs, and the full settings reference. For how providers fit together, see the [providers overview](/fleetplane/guides/providers/overview/).

## Walkthrough

1. **Create a personal access token** (with write scope) in the DigitalOcean control panel, ideally in a dedicated team/project.

2. **Export it** (the env var name is your choice — `secret://env/NAME` reads any variable):

   ```bash
   export DIGITALOCEAN_TOKEN=<token>
   ```

3. **Configure:**

   ```yaml
   providers:
     do-main:
       driver: digitalocean
       settings:
         token: secret://env/DIGITALOCEAN_TOKEN
         region: fra1            # default region; spec.location wins

   classes:
     workers:
       kind: compute.machine
       provider: do-main
       spec:
         serverType: s-2vcpu-4gb
         image: "slug:ubuntu-24-04-x64"
   ```

   Optional settings mirror Hetzner: `endpoint`, `rps`, `burst`, `maxConcurrent`.

4. **Image selector syntax:**

   | Form | Example | Selects |
   |---|---|---|
   | `id:<n>` | `id:123456` | An image by numeric ID |
   | `slug:<distro-slug>` | `slug:ubuntu-24-04-x64` | A distribution image by slug |
   | `snapshot:<name>` | `snapshot:ci-runner-v12` | Newest snapshot with that name |

   Unlike Hetzner, DigitalOcean snapshots do have names; with duplicate names the newest wins.

5. **Tag-based identity note.** DigitalOcean has flat tags instead of key=value labels, so Fleetplane encodes its reserved identity labels as tags of the form `fp-<short>:<value>` (e.g. `fp-id:...`, `fp-owner:...`, `fp-op:...`) on every droplet it manages. Do not remove these tags — ownership tracking, discovery, and crash recovery depend on them. Custom label keys outside the reserved set are dropped on DigitalOcean, and tag values are sanitized to the charset `[A-Za-z0-9:\-_]`.

## Credentials and permissions

Create a **personal access token** with read **and write** scopes (API → Tokens → Generate New Token). If you use custom scopes, the driver needs droplet create/read/delete plus tag access — it creates and deletes droplets and stamps identity tags on them. Prefer a dedicated team/project to bound what the token can see.

```yaml
settings:
  token: secret://env/DIGITALOCEAN_TOKEN
```

## Settings

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
[ADR-014](/fleetplane/developers/adr/adr-014-retries/)), and the same header-driven pacer shapes
traffic around DigitalOcean's rate limit.

## Sizes, images, and region

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

## Billing

The driver declares hourly billing with a `5m` termination buffer for
`compute.machine` only (droplets bill per started hour; the monthly cap is
deliberately not modeled — hourly is the conservative model for window
scheduling). With `reclaim.idleAfter` on a class, idle droplets are therefore
deleted in the safe window before their next billing boundary rather than
immediately when the idle window elapses; opt out per kind with
`disabled: true` under `providers.<name>.billing`
([configuration → billing](/fleetplane/guides/configuration/#billing-providersnamebilling)).

No [parking](/fleetplane/guides/concepts/#parked-machines-the-warm-tier): DigitalOcean bills
powered-off droplets at full price, so the driver declares no park capability
and delete-and-recreate stays the optimal reclaim path.

## Identity labels become `fp-*` tags

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

## Droplet status mapping

| Droplet status | Fleetplane observed phase |
|---|---|
| `new` | `pending` |
| `active` | `running` |
| `off` | `stopped` |
| `archive` | `gone` |
| anything else | `unknown` |
