---
title: "Installation"
description: "Install the binary, run it under systemd, or bootstrap a control VM with fleetplane cloud-init."
---

Fleetplane is one static binary: server, CLI and web dashboard. Install it, then run `fleetplane serve` under a supervisor — or let `fleetplane cloud-init` render the whole control-VM bootstrap for you.

## Install the binary

### Option A: install script (Linux / macOS)

Downloads the latest release binary for your platform, verifies its
checksum, and installs it (no Go toolchain needed):

```bash
curl -fsSL https://raw.githubusercontent.com/samishal1998/fleetplane/main/install.sh | sh
```

The script installs to `/usr/local/bin` when writable, else `~/.local/bin`.
Overrides: `FLEETPLANE_VERSION=v0.4.0` pins a release,
`FLEETPLANE_INSTALL_DIR=/some/bin` picks the directory. The script itself
lives in the repo ([`install.sh`](https://github.com/samishal1998/fleetplane/blob/main/install.sh)) — read it before piping if
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

## Run under systemd

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

:::note[Crash-safety note]
it is always safe to restart or `kill -9` Fleetplane. In-flight provider operations are journaled and resume on the next boot; mutating API requests are rejected with `503` (code `unready`) until recovery completes.
:::

### Or: bootstrap the control VM with cloud-init

`fleetplane cloud-init` renders everything in this section — packages, the
service user, `/etc/fleetplane/{config.yaml,secrets.env}`, the systemd unit,
the pinned install, `systemctl enable --now` — as one `#cloud-config` you
hand to your provider as user data when creating the control VM. Run it on
your workstation:

```bash
# secrets.env supplies every secret://env/NAME your config references
printf 'HETZNER_TOKEN=%s\n' "$HETZNER_TOKEN" > secrets.env

fleetplane cloud-init --config config.yaml --env secrets.env \
  --distro ubuntu --version v0.8.0 --out user-data.yaml
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

:::caution[Exposure note]
cloud-init user-data is readable from the provider's metadata
endpoint by any process on the VM, so the provider credentials in
`secrets.env` are visible to whatever runs there. That is acceptable for a
single-purpose control VM; do not reuse the VM for untrusted workloads.
Comments in your `config.yaml` do not survive the round trip.
:::

### fleetplane cloud-init reference

Render a `#cloud-config` user-data file that bootstraps a Fleetplane control VM:
installs the pinned release, writes `/etc/fleetplane/config.yaml` (with an API
token injected) and `/etc/fleetplane/secrets.env`, creates the service user, and
starts the systemd unit — the [setup guide §4](#run-under-systemd)
recipe, generated. Local only; no server call.

| Flag | Default | Description |
|---|---|---|
| `--config` | (required) | `config.yaml` to ship; re-validated after the token is injected |
| `--env` | — | `KEY=VALUE` file supplying `secret://env/NAME` references, repeatable; the current environment is the fallback |
| `--distro` | `ubuntu` | `ubuntu`, `debian`, `fedora`, `rhel`, `rocky`, `almalinux`, `centos`, `arch`, `opensuse` |
| `--version` | this CLI's version | Release tag to install; empty means latest |
| `--token` | — | Pre-generated `flp_…` token; only its digest ships. Omit to generate one (printed once to stderr) |
| `--token-name` | `admin` | Name for a generated token |
| `--perm` | `admin` | Permissions for a generated token, repeatable |
| `--out` | stdout | Output file (mode 0600) |

```bash
fleetplane cloud-init --config config.yaml --env secrets.env --distro debian --out user-data.yaml
```

Errors: a referenced `secret://env/NAME` with no value (all missing names listed),
an unsupported distro, a malformed `--token`, or a config that fails validation.
Warnings (stderr, non-fatal): `secret://file/` references (not shipped) and a
`storage.path` outside `/var/lib/fleetplane/`.

## Next steps

Write your `config.yaml` with the [configuration reference](/fleetplane/guides/configuration/), create API tokens ([tokens and permissions](/fleetplane/guides/operations/tokens/)), then pick a [provider](/fleetplane/guides/providers/overview/).
