---
title: "Architecture decision records"
description: "Every ADR: deviations from the design docs and decisions the docs left open."
sidebar:
  hidden: true
---

Deviations from the design docs (`docs/00`–`10`) and decisions the docs leave open are recorded here. One decision per file; status is `Accepted` unless noted.

| ADR | Decision |
|---|---|
| [ADR-001](/fleetplane/developers/adr/adr-001-go-version/) | Go 1.26 with pinned toolchain |
| [ADR-002](/fleetplane/developers/adr/adr-002-module-path/) | Module path and single-module layout |
| [ADR-003](/fleetplane/developers/adr/adr-003-sqlite-driver/) | modernc.org/sqlite as the storage driver |
| [ADR-004](/fleetplane/developers/adr/adr-004-ids/) | Prefixed ULIDs for all entity IDs |
| [ADR-005](/fleetplane/developers/adr/adr-005-http-router/) | stdlib ServeMux + colon-verb helper |
| [ADR-006](/fleetplane/developers/adr/adr-006-cli/) | cobra CLI as a pure HTTP client |
| [ADR-007](/fleetplane/developers/adr/adr-007-config/) | YAML config via goccy/go-yaml + `secret://` references |
| [ADR-008](/fleetplane/developers/adr/adr-008-tokens/) | API token format and SHA-256 storage |
| [ADR-009](/fleetplane/developers/adr/adr-009-boundaries/) | Mechanical boundary enforcement + the green gate |
| [ADR-010](/fleetplane/developers/adr/adr-010-migrations/) | goose migrations; PRAGMAs live in the DSN |
| [ADR-011](/fleetplane/developers/adr/adr-011-openapi/) | Hand-authored OpenAPI 3.0.3 with contract tests |
| [ADR-012](/fleetplane/developers/adr/adr-012-layout/) | Repo layout deviations from doc 09 |
| [ADR-013](/fleetplane/developers/adr/adr-013-registration-labels/) | Provider registration and reserved label keys |
| [ADR-014](/fleetplane/developers/adr/adr-014-retries/) | The operation engine is the only retry authority |
| [ADR-015](/fleetplane/developers/adr/adr-015-e2e-safety/) | Hetzner E2E safety protocol |
| [ADR-016](/fleetplane/developers/adr/adr-016-property-testing/) | Property-based testing with pgregory.net/rapid |
| [ADR-017](/fleetplane/developers/adr/adr-017-operation-states/) | Operation states, tombstone deletion, ghost vs orphan |
| [ADR-018](/fleetplane/developers/adr/adr-018-cost-aware-leasing/) | Cost-aware leasing and billing windows |
| [ADR-019](/fleetplane/developers/adr/adr-019-parked-machines/) | Parked machines (stop/resume warm tier) |
| [ADR-020](/fleetplane/developers/adr/adr-020-docker-provider/) | Docker as the credential-free end-to-end substrate |
| [ADR-API-001](/fleetplane/developers/adr/adr-api-001-read-endpoints/) | Additive read endpoints beyond doc 04 §3 |
| [ADR-API-002](/fleetplane/developers/adr/adr-api-002-operation-completeness/) | Completing the operation surface |
