# ADR-011: Hand-authored OpenAPI 3.0.3 with contract tests

Status: Accepted (2026-08-13)

## Decision

`api/openapi.yaml` is hand-written in OpenAPI **3.0.3** (the safer floor for third-party generators; 3.1's schema dialect is not interchangeable). No code generation — the ~18-route surface is too small to justify codegen churn.

Drift is prevented mechanically: a test diffs the server's `RouteDef` table against the spec in both directions, and the integration harness validates every response against the spec via `github.com/getkin/kin-openapi` (`openapi3filter`). Both are test-only dependencies.
