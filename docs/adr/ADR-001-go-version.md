# ADR-001: Go 1.26 with pinned toolchain

Status: Accepted (2026-08-13)

## Decision

`go 1.26.0` language directive + `toolchain go1.26.5` in `go.mod`. GOTOOLCHAIN auto-download keeps builds reproducible regardless of the locally installed Go. Upgrade policy: stay at most one release behind stable (Go 1.27 is in RC; adopt after it settles).

## Consequences

Features up to 1.26 are usable (ServeMux method+wildcard routing ≥1.22, `math/rand/v2` ≥1.22, `encoding/json` `omitzero` ≥1.24). CI resolves the version from `go.mod` via `actions/setup-go` `go-version-file`.
