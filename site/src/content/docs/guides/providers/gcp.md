---
title: "GCP"
description: "Run Fleetplane machines on Google Compute Engine: service account, image selectors, billing, parking and fp-* labels."
---

This page covers the `gcp` driver: a walkthrough from credentials to first machine, the permissions it needs, and the full settings reference. For how providers fit together, see the [providers overview](/fleetplane/guides/providers/overview/).

## Walkthrough

1. **Create a service account and key.** Create a dedicated service account with the permissions from [credentials and permissions](#credentials-and-permissions) (simple path: `roles/compute.instanceAdmin.v1` on the project), then create a JSON key for it and place it on the server:

   ```bash
   sudo install -m 0600 -o fleetplane sa-key.json /etc/fleetplane/gcp-sa.json
   ```

2. **Configure:**

   ```yaml
   providers:
     gcp-main:
       driver: gcp
       settings:
         project: my-project                                  # required
         zone: europe-west3-a                                 # required default zone; spec.location wins
         credentialsJson: secret://file/etc/fleetplane/gcp-sa.json
         # network: global/networks/default                   # optional; this is the default
         # subnetwork: regions/europe-west3/subnetworks/main  # optional

   classes:
     ci-gcp:
       kind: compute.machine
       provider: gcp-main
       spec:
         serverType: e2-medium
         image: "family:debian-cloud/debian-12"
   ```

   `credentialsJson` **must** be a `secret://` reference when set (a literal value is a boot error); `secret://file/...` is the natural fit for a JSON key. Omit it entirely to use Application Default Credentials instead (`GOOGLE_APPLICATION_CREDENTIALS`, gcloud user credentials, or the metadata server when Fleetplane runs on GCP) — no key in the config at all. `spec.location` is a zone. Optional pacing settings mirror the other drivers: `endpoint`, `rps`, `burst`, `maxConcurrent`.

3. **Image selector syntax:**

   | Form | Example | Selects |
   |---|---|---|
   | `id:<image>` | `id:my-image` | An image in your project by name; a self-link or partial URL passes through |
   | `family:<[project/]family>` | `family:debian-cloud/debian-12` | The latest image in a family (bare family name = your project) |
   | `name:<[project/]name>` | `name:debian-cloud/debian-12-bookworm-v20240101` | An exact image by name |
   | `snapshot:<k=v>` | `snapshot:ci-runner=v12` | Newest image in your project with that label |

   `snapshot:` selects images in your project by label — label the images your pipeline produces and select by label; the newest match wins. A selector matching nothing is a fail-fast config error, not a retry.

4. **Label-based identity note.** GCE labels only allow lowercase `[a-z0-9_-]` keys and values, so Fleetplane encodes its reserved identity labels as `fp-<short>` labels (e.g. `fp-id`, `fp-owner`, `fp-op`) on every instance it manages, with values lowercased on the way in and ULID values restored on the way out. Do not remove these labels — ownership tracking, discovery, and crash recovery depend on them. Custom label keys from `spec.labels` are sanitized to the GCE charset (lowercased; illegal runes become `-`).

5. **Try it:**

   ```bash
   fleetplane acquire --class ci-gcp --cpu 2 --ttl 90m
   fleetplane watch acq_01J...
   fleetplane resources
   ```

## Credentials and permissions

Create a **service account** for Fleetplane. The simple path: grant it `roles/compute.instanceAdmin.v1` on the project. Add `roles/iam.serviceAccountUser` **only** if you extend created instances to run as a service account — the driver does not attach one today, so it is normally unnecessary.

For a least-privilege custom role instead, these are the permissions behind the API calls the driver makes:

| Permission | Used for |
|---|---|
| `compute.instances.create` | Instance creation |
| `compute.instances.delete` | Instance deletion |
| `compute.instances.get` | Get by reference |
| `compute.instances.list` | Discovery (project-wide aggregated list) |
| `compute.instances.setLabels` | Identity labels on created instances |
| `compute.instances.setMetadata` | `spec.userData` (cloud-init via the `user-data` metadata key) |
| `compute.zoneOperations.get` | Polling create/delete operations |
| `compute.zones.get` | Provider health check |
| `compute.machineTypes.get` | Capacity (cpu/memory) reporting |
| `compute.images.get`, `compute.images.getFromFamily`, `compute.images.list`, `compute.images.useReadOnly` | Image selector resolution and boot-disk sourcing |
| `compute.disks.create` | The boot disk |
| `compute.subnetworks.use` | When `settings.subnetwork` is set |
| `compute.subnetworks.useExternalIp` (or `compute.networks.useExternalIp` on legacy networks) | The ephemeral external IP every instance gets |

GCP's mapping of permissions to API calls is theirs and can evolve; if a custom role fails with a 403 naming a permission, add it — or fall back to `roles/compute.instanceAdmin.v1`.

Create a JSON key for the service account and reference it via `secret://file`:

```yaml
settings:
  project: my-project
  zone: europe-west3-a
  credentialsJson: secret://file/etc/fleetplane/gcp-sa.json
```

**No-key alternative:** omit `credentialsJson` entirely to use Application Default Credentials — `GOOGLE_APPLICATION_CREDENTIALS`, gcloud user credentials, or the metadata server when Fleetplane runs on GCP with the service account attached to its own VM.

## Settings

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
[credentials and permissions](#credentials-and-permissions).
The REST client performs no automatic retries (the operation engine owns
them, [ADR-014](/fleetplane/developers/adr/adr-014-retries/)), and the shared pacer shapes
request rate.

## Machine types and images

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

## Billing

The driver declares GCE's per-second billing with a **60-second minimum** for
`compute.machine` — a minimum duration, no increment, exactly like the AWS
driver. No billing-boundary window scheduling results; override or disable
per kind via `providers.<name>.billing`
([configuration → billing](/fleetplane/guides/configuration/#billing-providersnamebilling)).

## Parking

The driver implements the optional `ParkAware` capability for
`compute.machine` ([concepts → parked machines](/fleetplane/guides/concepts/#parked-machines-the-warm-tier)):
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

## Identity labels become `fp-*` labels

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

## Instance status mapping

| GCE status | Fleetplane observed phase |
|---|---|
| `PENDING`, `PROVISIONING`, `STAGING` | `pending` |
| `RUNNING` | `running` |
| `PENDING_STOP`, `STOPPING`, `SUSPENDING`, `SUSPENDED`, `STOPPED`, `TERMINATED`, `DEPROVISIONING` | `stopped` |
| anything else (e.g. `REPAIRING`) | `unknown` |

Note GCE's `TERMINATED` means **stopped** — the instance still exists; a
deleted instance 404s instead (reported as the typed `not_found`). The full
native instance object is preserved in `status.extensions`.
