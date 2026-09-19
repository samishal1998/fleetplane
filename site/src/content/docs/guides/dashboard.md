---
title: "Web dashboard"
description: "The web UI embedded in the fleetplane binary: signing in, views, actions and security notes."
---

Fleetplane serves a built-in web dashboard from the same binary and listener as
the API. There is nothing to deploy, no build step, and no external requests —
the entire app (including its vendored Vue runtime) is embedded at compile time
and works offline.

## Opening it

Start the server and open the main API address in a browser:

```bash
fleetplane serve --config config.yaml
# then browse to http://127.0.0.1:8080/
```

`/` redirects to `/ui/`. The dashboard talks only to the same-origin `/v1` API —
exactly the endpoints the CLI uses.

## Signing in

- **Tokens configured** (`auth.tokens` in the config): paste an API token
  (`flp_<id8>.<secret>`) into the login screen. The token is kept in the
  browser's `localStorage` and sent as a `Authorization: Bearer` header on every
  request — it is never sent anywhere but this same-origin API.
- **No tokens configured**: the API is open (local development only; the server
  logs a loud warning) and the dashboard connects without a token.

What you can see and do is bounded by the token's permissions. Views a token
cannot read show an inline "lacks permission" note instead of data; actions the
token cannot perform fail with a visible error. Resolving uncertain operations
requires `provider.admin`.

## Views

| View | Shows | Actions |
|---|---|---|
| Overview | Stat tiles (resources, provisioning, **parked**, active + uncertain operations, pools, provider health), fleet-by-phase distribution, recent events | — |
| Resources | All live resources with phase, class, provider, external ID | Create (kind, provider, spec JSON, labels), open detail |
| Resource detail | Metadata, spec, capacity, provider extensions (verbatim, invariant 6), open operations, events, "Parked since" while parked | Park (shown on `ready` machines), Start (shown on `parked` machines), drain, undrain (shown on `draining` machines), protect / unprotect, delete (dry-run preview first, then journaled delete) |
| Pools | Declared pools with class, replicas, minReady | Create pool |
| Pool detail | Spec, generation, the pool's member resources | Scale replicas (±), edit spec JSON, reconcile now, pause / resume, delete pool |
| Classes | All classes (config + api) with kind, provider, source, policies | Create class (template validated on submit; policy fields: idle reclaim, park, delete-after-parked, queue budget), delete api-managed classes, view template |
| Acquisitions | Live acquisitions (`pending`, `provisioning`, `bound`) with a Live/All toggle, lookup by ID | Acquire (class, TTL, constraints, exclusive), release |
| Operations | **Open** (non-terminal) operations; terminal ones are visible in events | Resolve uncertain operations (`retry-verification` / `mark-failed`) |
| Events | Recent events, newest first, free-text filter | — |
| Providers | Per-instance health (state, last check, consecutive failures, last error) | — |

Every list refreshes automatically every ~2.5 s while the tab is visible.

Notes that follow from the API's semantics:

- **Delete is two-step**: the dashboard first calls `DELETE …?dryRun=true` and
  shows the server's answer before performing the real journaled delete with an
  idempotency key. Delete-protected resources cannot be deleted from the UI.
- **Park and Start** call `POST …:park` / `…:start`
  ([concepts → parked machines](/fleetplane/guides/concepts/#parked-machines-the-warm-tier)):
  Park appears only on `ready` machines, Start only on `parked` ones, and the
  detail view keeps refreshing while the machine transits `parking`/`starting`.
  The three warm-tier phases have their own colors in the phase distribution
  and phase badges.
- **The acquisitions list is live-only** by default, matching
  [`GET /v1/acquisitions`](/fleetplane/reference/http-api/#query-parameters): acquisitions are never
  garbage collected, so an unfiltered listing would be the control plane's
  entire history. The **All** toggle asks for the terminal states explicitly.
- **Pause and delete a pool** call `POST …:pause` / `…:resume` and
  `DELETE /v1/pools/{id}`. A paused pool converges for nothing until it is
  resumed, so its **Reconcile now** fails with a conflict; deleting one is
  refused while it still wants replicas or still has members.
- **The operations list is open-only** by design ([`GET /v1/operations`](/fleetplane/reference/http-api/)
  returns non-terminal operations); completed operations are audited through
  events and `GET /v1/operations/{id}`.
- **Uncertain operations** are surfaced with a ⚠ badge in the sidebar and an
  alert tile on the overview — see the [operations guide](/fleetplane/guides/operations/uncertain-operations/) for
  what "uncertain" means before resolving one.

## Theming

The dashboard follows the OS light/dark preference; the Auto/Light/Dark toggle
in the sidebar overrides it (persisted in the browser).

## Security notes

- The static assets under `/ui/` are public by design; **all fleet data still
  flows through the authenticated `/v1` API**. The page ships a strict
  same-origin Content-Security-Policy (`'unsafe-eval'` is allowed solely for
  Vue's in-browser template compiler).
- The dashboard is served on the **main** listener only. The ops listener
  (metrics, pprof, backup) stays loopback-only and is not reachable from the
  dashboard.
- Tokens pasted into the dashboard live in that browser's `localStorage`; use
  "Sign out" to clear them, and scope dashboard tokens to the permissions you
  actually need.

## Where it lives

`internal/webui/` — a stdlib-only package embedding `dist/` (`index.html`,
`app.js`, `style.css`, and the vendored `vue.global.prod.js`) via `go:embed`,
mounted at `/ui/` in `internal/boot`. Editing the dashboard is editing those
static files and rebuilding the binary; there is no Node toolchain in the
build.
