# ADR-009: Mechanical boundary enforcement + the green gate

Status: Accepted (2026-08-13)

## Boundaries

- `internal/**` and `pkg/kinds/**` never import `providers/*` or any cloud SDK.
- `providers/**` import only `pkg/sdk` (+ their own cloud SDK) — never `internal/`.
- `pkg/sdk/**` is stdlib-only (it is the contract surface third-party provider authors import).

Enforced twice: golangci-lint `depguard` (direct imports, in-editor feedback) and `scripts/check-boundaries.sh` (`go list -deps`, catches transitive leakage). The script fails closed and ships a `--self-test` mode proving each rule fires on planted violations.

## Green gate

Every increment ends with `make gate` green: build, vet, gofmt, `go mod tidy -diff`, `go test -race -shuffle=on -count=1`, boundaries, golangci-lint. Never accumulate red; a flaky test is quarantined by name in a tracked issue within one day or reverted with its feature.
