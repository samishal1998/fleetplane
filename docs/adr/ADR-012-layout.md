# ADR-012: Repo layout deviations from doc 09

Status: Accepted (2026-08-13)

## Deviations

- Storage implementations live under `internal/storage/sqlite` (doc 09 suggested top-level `storage/`): nothing external implements stores in v1, so they are not API surface.
- `cmd/fleetplane-build` is not created in v1 (doc 03 §5 build helper deferred); `cmd/fleetplane/modules.go` is the distribution seam.
- `pkg/sdk` contains exactly what third-party provider authors must import (contract types, `Register`, error model, conformance kit, secretref); `pkg/kinds` holds kind schemas. Everything orchestration-, storage-, or API-shaped is `internal/`.
- The design docs moved from the repo root to `docs/`.
