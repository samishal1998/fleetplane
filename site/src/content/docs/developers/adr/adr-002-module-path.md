---
title: "ADR-002: Module path and single-module layout"
description: "Module path and single-module layout."
sidebar:
  label: "002 · Module path and single-module layout"
---

Status: Accepted (2026-08-13)

## Decision

Module path `github.com/samishal1998/fleetplane` (confirmed by the project owner). One Go module for all of v1.

## Multi-module seam (doc 03 §5)

Providers import only `pkg/sdk` (+ their cloud SDK); `cmd/fleetplane/modules.go` is the only file that blank-imports providers. Extracting a provider into its own module later is a directory move + `go.mod`; a future `fleetplane-build` tool would merely generate a distribution file. `pkg/sdk` therefore stays stdlib-only (enforced, ADR-009).
