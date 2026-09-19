---
title: "Tokens and permissions"
description: "Mint API tokens, configure auth.tokens, and scope them with the closed permission set."
---

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

The closed permission set: `resource.read`, `resource.acquire`, `resource.create`, `resource.delete`, `pool.read`, `pool.write`, `class.read`, `class.write`, `provider.read`, `provider.admin`, `operation.read`, and `admin` (grants everything). Unknown permission names are boot errors. Details in [ADR-008](/fleetplane/developers/adr/adr-008-tokens/).

## How requests authenticate

Fleetplane uses static bearer tokens ([ADR-008](/fleetplane/developers/adr/adr-008-tokens/)):

```text
Authorization: Bearer flp_<id8>.<secret>
```

`<id8>` is an 8-hex-character lookup ID; `<secret>` is a base64url-encoded 256-bit random value. The server stores only `hex(sha256(secret))` in `auth.tokens` and compares in constant time.

Mint a token locally (no server call) and paste the printed YAML snippet into your config:

```bash
fleetplane token new --name ci --perm resource.acquire --perm resource.read
```

If **no tokens are configured**, authentication is disabled and the API runs open — for local development only. The server logs a loud warning at boot.

Failure modes:

- Missing/invalid token → `401` with code `unauthenticated` and a `WWW-Authenticate: Bearer` header.
- Valid token, missing permission → `403` with code `permission_denied` (message names the token and the missing permission).

### Permissions

Each route requires exactly one permission (see the route table below). The closed set:

| Permission | Grants |
|---|---|
| `resource.read` | Read resources and acquisitions |
| `resource.acquire` | Acquire and release capacity |
| `resource.create` | Create resources directly |
| `resource.delete` | Delete, drain, undrain, and protect resources |
| `pool.read` | Read pools |
| `pool.write` | Create, update, pause, reconcile, and delete pools |
| `class.read` | Read classes |
| `class.write` | Create, update, delete classes |
| `provider.read` | Read provider health |
| `provider.admin` | Resolve uncertain operations |
| `operation.read` | Read operations and events |
| `admin` | Everything |

## The auth block

Static API tokens ([ADR-008](/fleetplane/developers/adr/adr-008-tokens/)). With **no tokens
configured, the API runs open** — every request is allowed. That is for
local development only, and boot logs a loud warning. (The
[dashboard](/fleetplane/guides/dashboard/) likewise needs no token when the API is open.)

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

## fleetplane token new reference

Generate an API token **locally** — no server call, no server state. It prints the
plaintext secret exactly once, plus a ready-to-paste `auth.tokens` snippet for the
server config; the config stores only the SHA-256 of the secret
([ADR-008](/fleetplane/developers/adr/adr-008-tokens/)).

| Flag | Default | Description |
|---|---|---|
| `--name` | `default` | Token name (shown in audit events) |
| `--perm` | `admin` | Permission, repeatable |

```bash
fleetplane token new --name ci --perm resource.read --perm resource.acquire --perm operation.read
```

```text
token: flp_8f3a1c2d.Zkw3vWQx9pT4hK2mN8rB5cD1eF6gH0jL3nP7qS9uVaX

add to fleetplane config:

auth:
  tokens:
    - id: 8f3a1c2d
      name: ci
      sha256: 9c2f0e8a7b6d5c4e3f2a1b0c9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e
      permissions: [resource.read, resource.acquire, operation.read]
```

Valid permissions (closed set): `resource.read`, `resource.acquire`, `resource.create`,
`resource.delete`, `pool.read`, `pool.write`, `provider.read`, `provider.admin`,
`operation.read`, `admin` (allows everything). Unknown permissions are rejected — by
`token new` and again at server boot.

With no tokens configured, the server runs with authentication **disabled** (open API,
loud boot warning) and the CLI needs no `--token`.
