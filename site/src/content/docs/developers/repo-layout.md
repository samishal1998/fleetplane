---
title: "Repository layout and boundaries"
description: "Where code lives, and the mechanically enforced import boundaries between kernel, SDK and providers."
---

## Layout

```text
cmd/fleetplane/        the single binary: server, CLI and dashboard
  modules.go           the only file that blank-imports providers (the distribution seam)
internal/              everything orchestration-, storage- or API-shaped
  api/                 the HTTP API: route table, auth, handlers
  app/                 the application-services layer
  billing/             the pure billing-window math (one canonical paid(t))
  boot/                wires configuration, storage and the HTTP listeners
  capacity/            allocation arithmetic
  cli/                 the fleetplane command tree (pure HTTP clients)
  config/              loads and validates config.yaml
  ids/                 the single source of truth for ID prefixes
  lease/               lease lifecycle: release, TTL expiry sweeps
  metrics/             cost-aware-leasing and parking metrics
  operations/          the operation engine — the only caller of provider mutations
  phase/               the resource phase vocabulary and legal transitions
  provision/           the one create-journaling path
  reconcile/           the pool reconciler (and the discovery sweep)
  scheduler/           acquisition scheduling: filter, score, reserve
  storage/             the domain-shaped persistence contract
    sqlite/            its SQLite implementation
  webui/               the embedded dashboard (dist/ via go:embed)
pkg/
  sdk/                 what third-party provider authors import (stdlib-only)
    provider/          contract types, Register, error model
    conformance/       the provider conformance kit
    secretref/         secret:// resolution
  kinds/               kind schemas (compute.machine, storage.volume)
  apiclient/           the Go client of the HTTP API
providers/             hetzner, digitalocean, aws, gcp, docker, fake + shared pacing/
api/openapi.yaml       the hand-authored API contract
schema/                config.schema.json for editor autocomplete
scripts/               check-boundaries.sh, e2esweep (the E2E sweeper)
tests/                 cross-cutting integration, fault-injection, chaos and E2E tests
examples/              example config and demo.sh
docs/                  design docs, ADRs, guides, runbooks
site/                  this documentation site
```

## Deviations from the design docs

- Storage implementations live under `internal/storage/sqlite` (doc 09 suggested top-level `storage/`): nothing external implements stores in v1, so they are not API surface.
- `cmd/fleetplane-build` is not created in v1 (doc 03 §5 build helper deferred); `cmd/fleetplane/modules.go` is the distribution seam.
- `pkg/sdk` contains exactly what third-party provider authors must import (contract types, `Register`, error model, conformance kit, secretref); `pkg/kinds` holds kind schemas. Everything orchestration-, storage-, or API-shaped is `internal/`.
- The design docs moved from the repo root to `docs/`.

## Boundary rules

- `internal/**` and `pkg/kinds/**` never import `providers/*` or any cloud SDK.
- `providers/**` import only `pkg/sdk` (+ their own cloud SDK) — never `internal/`.
- `pkg/sdk/**` is stdlib-only (it is the contract surface third-party provider authors import).

Enforced twice: golangci-lint `depguard` (direct imports, in-editor feedback) and `scripts/check-boundaries.sh` (`go list -deps`, catches transitive leakage). The script fails closed and ships a `--self-test` mode proving each rule fires on planted violations.

## The multi-module seam

Providers import only `pkg/sdk` (+ their cloud SDK); `cmd/fleetplane/modules.go` is the only file that blank-imports providers. Extracting a provider into its own module later is a directory move + `go.mod`; a future `fleetplane-build` tool would merely generate a distribution file. `pkg/sdk` therefore stays stdlib-only (enforced, ADR-009).

Kernel purity (orchestration code never imports provider SDKs) is enforced mechanically — see `.golangci.yml` (depguard) and `scripts/check-boundaries.sh`.
