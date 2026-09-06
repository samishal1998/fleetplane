# Fleetplane Setup Guide

This guide takes you from nothing to a production Fleetplane deployment: install the binary, write a configuration file, secure the API with tokens, run under systemd, verify health, and connect a real cloud provider (Hetzner Cloud, DigitalOcean, AWS, or GCP).

Related reading: [configuration reference](docs/guides/configuration.md), [CLI reference](docs/guides/cli.md), [operations guide](docs/guides/operations.md), [providers guide](docs/guides/providers.md).

## 1. Install

### Option A: install script (Linux / macOS)

Downloads the latest release binary for your platform, verifies its
checksum, and installs it (no Go toolchain needed):

```bash
curl -fsSL https://raw.githubusercontent.com/samishal1998/fleetplane/main/install.sh | sh
```

The script installs to `/usr/local/bin` when writable, else `~/.local/bin`.
Overrides: `FLEETPLANE_VERSION=v0.4.0` pins a release,
`FLEETPLANE_INSTALL_DIR=/some/bin` picks the directory. The script itself
lives in the repo ([`install.sh`](install.sh)) — read it before piping if
that's your policy. Prebuilt binaries cover linux/amd64, linux/arm64,
darwin/amd64, darwin/arm64.

### Option B: `go install`

Requires Go 1.26+:

```bash
go install github.com/samishal1998/fleetplane/cmd/fleetplane@latest
```

The binary lands in `$(go env GOPATH)/bin/fleetplane`. Note `fleetplane version` prints `dev` for locally built binaries — release builds inject the version via `-ldflags "-X main.version=..."`.

### Option C: build from source

```bash
git clone https://github.com/samishal1998/fleetplane
cd fleetplane
go build -o fleetplane ./cmd/fleetplane
sudo install -m 0755 fleetplane /usr/local/bin/fleetplane
```

Verify:

```bash
fleetplane version
# fleetplane dev (go1.26.5)
```

## 2. Write config.yaml

Fleetplane reads one YAML file, passed to `fleetplane serve --config PATH`. Decoding is **strict**: unknown keys are boot errors, so typos fail fast instead of being silently ignored. All durations are Go duration strings (`"20s"`, `"5m"`, `"1h30m"`).

Start from [`examples/config.yaml`](examples/config.yaml) and build it up block by block.

### 2.1 Server

```yaml
server:
  addr: ":8080"              # main API + dashboard listener
  opsAddr: "127.0.0.1:9090"  # metrics, pprof, admin backup — keep loopback-only
  shutdownGrace: 20s         # graceful shutdown bound
```

All three have exactly these defaults, so you can omit the block entirely. The ops listener serves `/metrics`, `/debug/pprof/`, and `POST /admin/backup` with **no authentication** — never expose it on an untrusted network.

### 2.2 Storage

```yaml
storage:
  path: /var/lib/fleetplane/fleetplane.db
```

`storage.path` is the only required key in the whole file — boot fails with `storage.path is required` without it. Storage is a single SQLite file in WAL mode; put it on durable local disk and make sure the service user can write the directory.

### 2.3 Providers

Each entry under `providers` is a named provider *instance* with a `driver` and driver-specific `settings`. Credentials must always be `secret://` references — plaintext secrets never belong in the config file:

- `secret://env/NAME` — read from the environment variable `NAME` at boot
- `secret://file/absolute/path` — read from a file (one trailing newline is trimmed)

```yaml
providers:
  hetzner-main:
    driver: hetzner
    settings:
      token: secret://env/HETZNER_TOKEN
      location: fsn1
```

Available drivers: `hetzner`, `digitalocean`, `aws`, `gcp`, `docker` (containers on a local Docker Engine — a real provider with no credentials), and `fake` (deterministic in-memory provider, ideal for trying Fleetplane without a cloud account). See sections 8–11 for full provider walkthroughs and section 12 for the credentials and permissions each provider needs.

### 2.4 Classes

Classes are reusable creation templates. Acquisitions and pools reference a class instead of repeating machine specs:

```yaml
classes:
  ci-large:
    kind: compute.machine
    provider: hetzner-main
    spec:
      serverType: cpx31
      image: "snapshot:ci-runner=v12"
      location: fsn1
    reclaim:
      idleAfter: 5m        # delete idle poolless machines created from this class
```

A class needs `kind` and `provider`, and `provider` must name a configured provider instance — both are validated at boot. For `compute.machine`, `spec.serverType` and `spec.image` are required; `spec` may also carry provider-specific fields.

### 2.5 Tuning (all optional)

| Key | Default | Meaning |
|---|---|---|
| `engine.pollInterval` | `2s` | Operation engine wake-up cadence |
| `engine.verifyWindow` | `120s` | How long to verify uncertain creates before freezing them |
| `reconcile.interval` | `15s` | Pool reconciler period |
| `reconcile.maxMutationsPerCycle` | `5` | Max creates/deletes per reconcile cycle (the one budget knob) |
| `acquire.pendingTimeout` | `15m` | Expire acquisitions that were never satisfied |
| `discovery.interval` | `30s` | Provider discovery sweep period |
| `discovery.orphanGrace` | `60s` | Grace between orphan confirmation and tombstone |
| `discovery.ghostPolicy` | `delete` | Ghost handling: `delete` or `surface` |
| `discovery.adoptUnlabeled` | `off` | Unlabeled-resource adoption: `off` or `observed` |

The defaults are production-sane; leave the blocks out until you have a reason not to.

## 3. API tokens

With no tokens configured the API runs **open** and boot logs a loud warning — fine on a laptop, not in production.

Generate a token locally (no server call; the plaintext is shown exactly once):

```bash
fleetplane token new --name ci --perm resource.read --perm resource.acquire --perm operation.read
```

```text
token: flp_8f3a1c2d.<base64-secret>

add to fleetplane config:

auth:
  tokens:
    - id: 8f3a1c2d
      name: ci
      sha256: <64 hex chars>
      permissions: [resource.read, resource.acquire, operation.read]
```

Paste the printed snippet into `config.yaml` and restart the server. The config stores only `hex(sha256(secret))` — the server never sees or keeps the plaintext, so losing it means generating a new token. Defaults: `--name default`, `--perm admin`.

Clients send the token as `Authorization: Bearer flp_<id>.<secret>`; the CLI takes it from `--token` or the `FLEETPLANE_TOKEN` environment variable:

```bash
export FLEETPLANE_ADDR=http://fleetplane.internal:8080
export FLEETPLANE_TOKEN=flp_8f3a1c2d.<base64-secret>
fleetplane resources
```

The closed permission set: `resource.read`, `resource.acquire`, `resource.create`, `resource.delete`, `pool.read`, `pool.write`, `class.read`, `class.write`, `provider.read`, `provider.admin`, `operation.read`, and `admin` (grants everything). Unknown permission names are boot errors. Details in [ADR-008](docs/adr/ADR-008-tokens.md).

## 4. Run under systemd

```bash
sudo useradd --system --home /var/lib/fleetplane --shell /usr/sbin/nologin fleetplane
sudo mkdir -p /var/lib/fleetplane /etc/fleetplane
sudo chown fleetplane:fleetplane /var/lib/fleetplane
```

Put provider credentials in an environment file readable only by root:

```bash
sudo tee /etc/fleetplane/secrets.env >/dev/null <<'EOF'
HETZNER_TOKEN=<your hetzner api token>
EOF
sudo chmod 0600 /etc/fleetplane/secrets.env
```

`/etc/systemd/system/fleetplane.service`:

```ini
[Unit]
Description=Fleetplane control plane
After=network-online.target
Wants=network-online.target

[Service]
User=fleetplane
Group=fleetplane
EnvironmentFile=/etc/fleetplane/secrets.env
ExecStart=/usr/local/bin/fleetplane serve --config /etc/fleetplane/config.yaml
Restart=always
RestartSec=2
# First SIGTERM triggers graceful shutdown; allow more than server.shutdownGrace (default 20s).
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now fleetplane
sudo journalctl -u fleetplane -f    # JSON logs on stderr
```

Crash-safety note: it is always safe to restart or `kill -9` Fleetplane. In-flight provider operations are journaled and resume on the next boot; mutating API requests are rejected with `503` (code `unready`) until recovery completes.

### 4.1 Or: bootstrap the control VM with cloud-init

`fleetplane cloud-init` renders everything in this section — packages, the
service user, `/etc/fleetplane/{config.yaml,secrets.env}`, the systemd unit,
the pinned install, `systemctl enable --now` — as one `#cloud-config` you
hand to your provider as user data when creating the control VM. Run it on
your workstation:

```bash
# secrets.env supplies every secret://env/NAME your config references
printf 'HETZNER_TOKEN=%s\n' "$HETZNER_TOKEN" > secrets.env

fleetplane cloud-init --config config.yaml --env secrets.env \
  --distro ubuntu --version v0.7.0 --out user-data.yaml
# token (admin, shown once): flp_8f3a1c2d.Zkw3vWQx…    ← printed to stderr
```

Then `hcloud server create --user-data-from-file user-data.yaml …`, or paste
it into the cloud console's user-data field. On first boot the VM installs the
release, starts the service, and your token works against `http://<vm>:8080`.

What the command checks for you:

- The shipped config is re-validated **after** the token is injected — the
  exact bytes that land on the VM pass the same strict parse as `serve`.
- Every `secret://env/NAME` the config references must be satisfied by an
  `--env` file (KEY=VALUE lines) or your current environment; missing names
  are one error. `secret://file/...` references are warned about — files are
  not shipped, place them on the VM yourself.
- A `storage.path` outside `/var/lib/fleetplane/` is warned about (the
  service user owns only that directory).

Token: pass a pre-generated one with `--token flp_…` (only its SHA-256 goes
into the config) or omit it and one is minted with `--token-name`/`--perm`
and printed once to stderr. The plaintext never appears in the user-data.

Distros (`--distro`): `ubuntu`, `debian`, `fedora`, `rhel`, `rocky`,
`almalinux`, `centos`, `arch`, `opensuse` — all systemd-based. The only real
difference is packages: the Fedora/RHEL family ships `curl-minimal`, which
conflicts with `curl`, so it is not installed there. Ubuntu, Debian, AlmaLinux,
Arch and openSUSE package lists were verified in containers; SELinux-enforcing
hosts and Alpine (no systemd) are untested/unsupported.

Exposure note: cloud-init user-data is readable from the provider's metadata
endpoint by any process on the VM, so the provider credentials in
`secrets.env` are visible to whatever runs there. That is acceptable for a
single-purpose control VM; do not reuse the VM for untrusted workloads.
Comments in your `config.yaml` do not survive the round trip.

## 5. Verify

Health endpoints are served on both listeners:

```bash
curl -s http://127.0.0.1:8080/health/live    # {"status":"ok"} — always, once the process is up
curl -s http://127.0.0.1:8080/health/ready   # {"status":"ok"} — after storage ping + journal recovery
```

Readiness reflects storage and journal recovery only; provider outages never make Fleetplane unready.

Metrics (ops listener):

```bash
curl -s http://127.0.0.1:9090/metrics | grep '^fleetplane_'
# fleetplane_resources{provider,kind,phase}
# fleetplane_leases_active
# fleetplane_operations{state}        <- alert on state="uncertain" > 0
# fleetplane_provider_health_state{provider,state}
# fleetplane_pools
```

Provider health from the CLI:

```bash
fleetplane providers
# INSTANCE      DRIVER   STATE    FAILURES  LAST ERROR
# hetzner-main  hetzner  healthy  0
```

## 6. Open the dashboard

The web dashboard is embedded in the binary and served by `fleetplane serve` itself at `http://<server.addr>/ui/` (`/` redirects there). No extra deployment; it works offline.

- **Login:** paste an API token; it is stored in your browser's localStorage. With no tokens configured the API is open and the dashboard needs no token.
- **Views:** Overview (fleet stats, provider health, recent events), Resources, Pools, Acquisitions, Operations, Events, Providers.
- **Actions:** create resources, delete with dry-run preview, drain, create/update/reconcile pools, acquire/release, resolve uncertain operations.

See the [dashboard guide](docs/guides/dashboard.md).

## 7. Create pools

Declare desired fleet size and let the reconciler converge — via the declarative CLI:

```bash
fleetplane apply -f - <<'EOF'
apiVersion: fleetplane.io/v1alpha1
kind: Pool
metadata:
  name: ci-runners
spec:
  class: ci-large
  replicas: 3
EOF

fleetplane pools                       # list
fleetplane pools get pool_01J...       # inspect one
fleetplane pools reconcile pool_01J... # nudge reconciliation now
```

`fleetplane apply -f` accepts multi-document YAML (or JSON) with kinds `Pool` and `Resource`; re-applying the same file is idempotent (Resource applies are create-only, keyed by `metadata.name`). Equivalent imperative routes exist for automation: `POST /v1/pools` and `PUT /v1/pools/{id}` (permission `pool.write`), or `fleetplane pools apply -f pool.json` with a JSON manifest. Pool spec fields: `class`, `replicas`, plus optional `minReady`, `maxResources`, `reclaim.idleAfter`, or an inline `machine` spec with `provider` and `kind` instead of `class`.

## 8. Hetzner Cloud walkthrough

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

## 9. DigitalOcean walkthrough

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

## 10. AWS walkthrough

1. **Create IAM credentials.** Create a dedicated IAM user (or role) with the minimal EC2 policy from [section 12](#12-provider-credentials-and-permissions), then create an access key for it. Alternatively, skip static keys entirely: when Fleetplane itself runs on AWS, attach the policy to the instance's role and omit `accessKeyId`/`secretAccessKey` — the driver falls back to the SDK's ambient credential chain (environment, shared config/credentials files, IMDS/IRSA roles), and no keys go in the config at all.

2. **Export the key pair** where the server runs (or put it in the systemd `EnvironmentFile`):

   ```bash
   export AWS_ACCESS_KEY_ID=<access key id>
   export AWS_SECRET_ACCESS_KEY=<secret access key>
   ```

3. **Configure:**

   ```yaml
   providers:
     aws-main:
       driver: aws
       settings:
         region: eu-central-1                              # required
         accessKeyId: secret://env/AWS_ACCESS_KEY_ID       # omit BOTH keys for the ambient chain
         secretAccessKey: secret://env/AWS_SECRET_ACCESS_KEY
         # sessionToken: secret://env/AWS_SESSION_TOKEN    # only with temporary credentials
         # subnetId: subnet-0abc123                        # optional placement
         # securityGroupIds: [sg-0abc123]                  # optional
         # keyName: my-keypair                             # optional EC2 key pair
         # instanceProfile: my-profile                     # optional; needs iam:PassRole (section 12)

   classes:
     ci-aws:
       kind: compute.machine
       provider: aws-main
       spec:
         serverType: t3.medium
         image: "id:ami-0abc1234567890def"
   ```

   `accessKeyId` and `secretAccessKey` are all-or-nothing — setting only one is a boot error, as is a `sessionToken` without the pair. `spec.location` is an availability zone (e.g. `eu-central-1a`). Optional pacing settings mirror the other drivers: `endpoint`, `rps`, `burst`, `maxConcurrent`.

4. **Image selector syntax:**

   | Form | Example | Selects |
   |---|---|---|
   | `id:<ami>` | `id:ami-0abc1234567890def` | An AMI by ID |
   | `name:<pattern>` | `name:ubuntu/images/hvm-ssd-gp3/*24.04*` | Newest available self- or Amazon-owned AMI matching the name pattern |
   | `snapshot:<k=v>` | `snapshot:ci-runner=v12` | Newest available self-owned AMI with that tag |

   `snapshot:` selects your own AMIs by tag equality — tag the AMIs your image pipeline produces (e.g. `ci-runner=v12`) and select by tag. When several match, the newest wins. A selector matching nothing is a fail-fast config error, not a retry.

5. **Try it:**

   ```bash
   fleetplane acquire --class ci-aws --cpu 2 --ttl 90m
   fleetplane watch acq_01J...
   fleetplane resources
   ```

AWS tag keys permit dots and slashes, so the `fleetplane.io/*` identity labels land on instances as native EC2 tags, verbatim. Leave them in place — discovery and crash recovery depend on them.

## 11. GCP walkthrough

1. **Create a service account and key.** Create a dedicated service account with the permissions from [section 12](#12-provider-credentials-and-permissions) (simple path: `roles/compute.instanceAdmin.v1` on the project), then create a JSON key for it and place it on the server:

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

## 12. Provider credentials and permissions

What credential each driver needs, and the minimal permissions to grant it. Console navigation reflects the providers' current UIs and may drift — the permission lists are the stable part.

### Hetzner Cloud

Create a **project-scoped API token** with **Read & Write** permission: in the Hetzner Cloud console, select (ideally) a dedicated project, then Security → API tokens → Generate API token. Hetzner tokens have no finer-grained scoping than read vs. read/write; write is required because Fleetplane creates and deletes servers. Using a dedicated project is the blast-radius limit: the token can only touch that project's resources.

```yaml
settings:
  token: secret://env/HETZNER_TOKEN
```

### DigitalOcean

Create a **personal access token** with read **and write** scopes (API → Tokens → Generate New Token). If you use custom scopes, the driver needs droplet create/read/delete plus tag access — it creates and deletes droplets and stamps identity tags on them. Prefer a dedicated team/project to bound what the token can see.

```yaml
settings:
  token: secret://env/DIGITALOCEAN_TOKEN
```

### AWS

Create an **IAM user or role** with this minimal policy — everything the driver calls, nothing more:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "FleetplaneEC2",
      "Effect": "Allow",
      "Action": [
        "ec2:RunInstances",
        "ec2:TerminateInstances",
        "ec2:DescribeInstances",
        "ec2:DescribeImages",
        "ec2:DescribeInstanceTypes",
        "ec2:DescribeRegions",
        "ec2:CreateTags"
      ],
      "Resource": "*"
    }
  ]
}
```

`ec2:DescribeRegions` backs the provider health check; `ec2:CreateTags` is required because every create tags the instance with the `fleetplane.io/*` identity labels. If you want to tighten `ec2:CreateTags` so the credential can only tag at instance creation (never re-tag existing resources), split it into its own statement with the condition `"StringEquals": {"ec2:CreateAction": "RunInstances"}` — kept simple by default above.

Add `iam:PassRole` **only** when `settings.instanceProfile` is configured — launching an instance with an instance profile passes its role:

```json
{
  "Sid": "FleetplanePassRole",
  "Effect": "Allow",
  "Action": "iam:PassRole",
  "Resource": "arn:aws:iam::<account-id>:role/<instance-profile-role>"
}
```

Wire the key pair through `secret://env`:

```yaml
settings:
  region: eu-central-1
  accessKeyId: secret://env/AWS_ACCESS_KEY_ID
  secretAccessKey: secret://env/AWS_SECRET_ACCESS_KEY
```

**No-keys alternative:** when Fleetplane itself runs on AWS, attach the policy to the instance's IAM role and omit `accessKeyId`/`secretAccessKey` entirely — the driver uses the SDK's ambient credential chain (environment variables, shared config/credentials files, IMDS/IRSA roles) and no secret ever appears in the config.

### GCP

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

## 13. Backup and restore

Hot backup runs online via the ops listener (`VACUUM INTO` on a dedicated connection — safe under WAL, and kernel writes never queue behind it):

```bash
fleetplane admin backup --to /var/backups/fleetplane/fleetplane-$(date +%F).db
```

The `--to` path is on the **server** host (the CLI just POSTs `{"to":PATH}` to `<ops-addr>/admin/backup`); `--ops-addr` defaults to `$FLEETPLANE_OPS_ADDR`, else `http://127.0.0.1:9090`. Full procedure, including restore: [backup and restore runbook](docs/runbooks/backup-restore.md).

## 14. Troubleshooting

Boot fails fast on configuration problems. Common errors:

| Symptom | Cause | Fix |
|---|---|---|
| `config: ... unknown field` | A key the binary does not implement (often a typo) — decoding is strict | Fix the key name; compare with [`examples/config.yaml`](examples/config.yaml) |
| `config: storage.path is required` | Missing `storage.path` | Set the SQLite file path |
| `config: invalid duration "..."` | Duration not a Go duration string | Use forms like `20s`, `5m`, `1h30m` |
| `config: providers.<name>.driver is required` | Provider block without `driver` | Set `driver: hetzner`, `digitalocean`, `aws`, `gcp`, or `fake` |
| `config: classes.<name> needs kind and provider` | Incomplete class | Add `kind` and `provider` |
| `config: classes.<name> references unknown provider "..."` | Class points at an unconfigured provider | Match the class `provider` to a `providers` entry |
| `config: auth.tokens[N] needs id and sha256` | Incomplete token entry | Paste the full snippet from `fleetplane token new` |
| `config: auth.tokens[N].sha256 must be 64 hex chars` | Truncated or plaintext value in `sha256` | Store `hex(sha256(secret))`, not the secret |
| `config: auth.tokens: duplicate id "..."` | Two tokens share an `id` | Generate a fresh token |
| `auth.tokens[N]: unknown permission "..."` | Permission name outside the closed set | Use the names listed in section 3 |
| `hetzner: token is required (secret:// reference)` (same for `digitalocean:`) | Missing/empty provider token setting | Set `settings.token` to a `secret://` reference |
| `aws: region is required` / `gcp: project is required` / `gcp: zone is required` | Missing required driver setting | Set `settings.region` (aws) or `settings.project` + `settings.zone` (gcp) |
| `aws: accessKeyId and secretAccessKey are all-or-nothing` | Only one of the static key pair set | Set both, or omit both to use the ambient credential chain |
| `gcp: credentialsJson must be a secret:// reference (07 §4)` | Literal value in `credentialsJson` | Use `secret://file/...` (or omit it to use Application Default Credentials) |
| `environment variable "NAME" is not set` | `secret://env/NAME` points at an unset variable | Export it in the service environment (systemd `EnvironmentFile`) |
| `listen <addr>: ... address already in use` | Another process holds `server.addr` or `server.opsAddr` | Free the port or change the address |
| `NO API TOKENS CONFIGURED` warning in logs | `auth.tokens` is empty — the API is open | Add tokens (section 3) before exposing the listener |
| Mutating requests return `503` code `unready` | Journal recovery still running, or shutdown drain | Wait and retry (the response carries `Retry-After`); check `/health/ready` |
| `fleetplane_operations{state="uncertain"} > 0` | An operation exhausted verification and is frozen | Inspect with `fleetplane operations`, then resolve via `POST /v1/operations/{id}:resolve` or the dashboard — see the [operations guide](docs/guides/operations.md) |
