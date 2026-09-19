# Architecture Decision Records

Deviations from the design docs (`docs/00`–`10`) and decisions the docs leave open are recorded here. One decision per file; status is `Accepted` unless noted.

| ADR | Decision |
|---|---|
| [001](ADR-001-go-version.md) | Go 1.26 + toolchain pinning |
| [002](ADR-002-module-path.md) | Module path, single module for v1 |
| [003](ADR-003-sqlite-driver.md) | modernc.org/sqlite (pure Go) |
| [004](ADR-004-ids.md) | Prefixed ULIDs |
| [005](ADR-005-http-router.md) | stdlib ServeMux + colon-verb helper |
| [006](ADR-006-cli.md) | cobra CLI as a pure HTTP client |
| [007](ADR-007-config.md) | YAML config (goccy/go-yaml) + `secret://` refs |
| [008](ADR-008-tokens.md) | API token format + SHA-256 storage |
| [009](ADR-009-boundaries.md) | Kernel-purity enforcement + green gate |
| [010](ADR-010-migrations.md) | goose migrations, PRAGMAs in DSN only |
| [011](ADR-011-openapi.md) | Hand-authored OpenAPI 3.0.3 + contract tests |
| [012](ADR-012-layout.md) | Repo layout deviations from doc 09 |
| [013](ADR-013-registration-labels.md) | Provider registration + reserved label keys |
| [014](ADR-014-retries.md) | Operation engine is the only retry authority |
| [015](ADR-015-e2e-safety.md) | Hetzner E2E safety protocol |
| [016](ADR-016-property-testing.md) | pgregory.net/rapid (MPL-2.0, test-only) |
| [017](ADR-017-operation-states.md) | Operation states; tombstone deletion; ghost vs orphan |
| [API-001](ADR-API-001-read-endpoints.md) | Additive read endpoints beyond 04 §3 |
| [API-002](ADR-API-002-operation-completeness.md) | Completing the operation surface (pause, protect, pool delete, undrain) |
