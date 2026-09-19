---
title: "Building and the green gate"
description: "Build from source, run make gate, and what CI runs."
---

## Build from source

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

The binary lands in `$(go env GOPATH)/bin/fleetplane`. Note `fleetplane version` prints `dev` for locally built binaries — release builds inject the version via `-ldflags "-X main.version=..."`.

## The green gate

```bash
make gate    # build + vet + fmt-check + tidy-check + test(-race) + boundaries + lint
```

Every increment must end with `make gate` green ([ADR-009](/fleetplane/developers/adr/adr-009-boundaries/)). Individual targets: `make build`, `make vet`, `make fmt-check`, `make tidy-check`, `make test` (runs `go test -race -shuffle=on -count=1 ./...`), `make boundaries`, `make lint` (and `make lint-install` for golangci-lint).

### The rule

Every increment ends with `make gate` green: build, vet, gofmt, `go mod tidy -diff`, `go test -race -shuffle=on -count=1`, boundaries, golangci-lint. Never accumulate red; a flaky test is quarantined by name in a tracked issue within one day or reverted with its feature.

### Make targets

| Target | Runs |
|---|---|
| `make gate` | `build vet fmt-check tidy-check test boundaries lint`, in that order |
| `make build` | `go build ./...` |
| `make vet` | `go vet ./...` |
| `make fmt-check` | fails if `gofmt -l .` lists any file |
| `make tidy-check` | `go mod tidy -diff` |
| `make test` | `go test -race -shuffle=on -count=1 ./...` |
| `make boundaries` | `scripts/check-boundaries.sh --self-test`, then `scripts/check-boundaries.sh` |
| `make lint` | `golangci-lint run ./...` (skipped with a note when golangci-lint is not installed; CI runs it) |
| `make lint-install` | installs golangci-lint into `$(go env GOPATH)/bin` |

## Toolchain

`go 1.26.0` language directive + `toolchain go1.26.5` in `go.mod`. GOTOOLCHAIN auto-download keeps builds reproducible regardless of the locally installed Go. Upgrade policy: stay at most one release behind stable (Go 1.27 is in RC; adopt after it settles).

## CI

`.github/workflows/ci.yml` runs on pushes to `main`, pull requests, a nightly schedule and manual dispatch. Its jobs mirror the gate: `build` (build, vet, gofmt, tidy), `test` (race-enabled tests with `FLEETPLANE_E2E_DOCKER=1`, so the Docker end-to-end tests cannot silently skip), `lint` (golangci-lint), `boundaries`, and `conformance` (the provider conformance catalogue against the fake and Docker drivers). The live Hetzner E2E job runs only when a token is available — see [testing](/fleetplane/developers/testing/#live-hetzner-e2e).
