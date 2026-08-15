# Fleetplane Setup Guide

This guide takes you from nothing to a production Fleetplane deployment: install the binary, write a configuration file, secure the API with tokens, run under systemd, verify health, and connect a real cloud provider (Hetzner Cloud or DigitalOcean).

Related reading: [configuration reference](docs/guides/configuration.md), [CLI reference](docs/guides/cli.md), [operations guide](docs/guides/operations.md), [providers guide](docs/guides/providers.md).

## 1. Install

### Option A: `go install`

Requires Go 1.26+:

```bash
go install github.com/samimishal/fleetplane/cmd/fleetplane@latest
```

The binary lands in `$(go env GOPATH)/bin/fleetplane`. Note `fleetplane version` prints `dev` for locally built binaries — release builds inject the version via `-ldflags "-X main.version=..."`.

### Option B: build from source

```bash
git clone https://github.com/samimishal/fleetplane
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

Available drivers: `hetzner`, `digitalocean`, and `fake` (deterministic in-memory provider, ideal for trying Fleetplane without a cloud account). See sections 8 and 9 for full provider walkthroughs.

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

The closed permission set: `resource.read`, `resource.acquire`, `resource.create`, `resource.delete`, `pool.read`, `pool.write`, `provider.read`, `provider.admin`, `operation.read`, and `admin` (grants everything). Unknown permission names are boot errors. Details in [ADR-008](docs/adr/ADR-008-tokens.md).

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

## 10. Backup and restore

Hot backup runs online via the ops listener (`VACUUM INTO` on a dedicated connection — safe under WAL, and kernel writes never queue behind it):

```bash
fleetplane admin backup --to /var/backups/fleetplane/fleetplane-$(date +%F).db
```

The `--to` path is on the **server** host (the CLI just POSTs `{"to":PATH}` to `<ops-addr>/admin/backup`); `--ops-addr` defaults to `$FLEETPLANE_OPS_ADDR`, else `http://127.0.0.1:9090`. Full procedure, including restore: [backup and restore runbook](docs/runbooks/backup-restore.md).

## 11. Troubleshooting

Boot fails fast on configuration problems. Common errors:

| Symptom | Cause | Fix |
|---|---|---|
| `config: ... unknown field` | A key the binary does not implement (often a typo) — decoding is strict | Fix the key name; compare with [`examples/config.yaml`](examples/config.yaml) |
| `config: storage.path is required` | Missing `storage.path` | Set the SQLite file path |
| `config: invalid duration "..."` | Duration not a Go duration string | Use forms like `20s`, `5m`, `1h30m` |
| `config: providers.<name>.driver is required` | Provider block without `driver` | Set `driver: hetzner`, `digitalocean`, or `fake` |
| `config: classes.<name> needs kind and provider` | Incomplete class | Add `kind` and `provider` |
| `config: classes.<name> references unknown provider "..."` | Class points at an unconfigured provider | Match the class `provider` to a `providers` entry |
| `config: auth.tokens[N] needs id and sha256` | Incomplete token entry | Paste the full snippet from `fleetplane token new` |
| `config: auth.tokens[N].sha256 must be 64 hex chars` | Truncated or plaintext value in `sha256` | Store `hex(sha256(secret))`, not the secret |
| `config: auth.tokens: duplicate id "..."` | Two tokens share an `id` | Generate a fresh token |
| `auth.tokens[N]: unknown permission "..."` | Permission name outside the closed set | Use the names listed in section 3 |
| `hetzner: token is required (secret:// reference)` (same for `digitalocean:`) | Missing/empty provider token setting | Set `settings.token` to a `secret://` reference |
| `environment variable "NAME" is not set` | `secret://env/NAME` points at an unset variable | Export it in the service environment (systemd `EnvironmentFile`) |
| `listen <addr>: ... address already in use` | Another process holds `server.addr` or `server.opsAddr` | Free the port or change the address |
| `NO API TOKENS CONFIGURED` warning in logs | `auth.tokens` is empty — the API is open | Add tokens (section 3) before exposing the listener |
| Mutating requests return `503` code `unready` | Journal recovery still running, or shutdown drain | Wait and retry (the response carries `Retry-After`); check `/health/ready` |
| `fleetplane_operations{state="uncertain"} > 0` | An operation exhausted verification and is frozen | Inspect with `fleetplane operations`, then resolve via `POST /v1/operations/{id}:resolve` or the dashboard — see the [operations guide](docs/guides/operations.md) |
