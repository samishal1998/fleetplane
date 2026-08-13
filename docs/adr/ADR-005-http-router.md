# ADR-005: stdlib ServeMux + colon-verb helper

Status: Accepted (2026-08-13)

## Decision

`net/http.ServeMux` (Go ≥1.22 method + wildcard patterns); no router dependency. Colon verbs from doc 04 §3 (`POST /v1/resources/{id}:drain`) cannot be expressed as ServeMux patterns (wildcards span whole segments), so they register as `POST /v1/resources/{idverb}` and a helper splits the segment at the last `:` (IDs contain no colon); unknown verbs 404.

A single `RouteDef` table drives mux registration, per-route permissions, the mutating flag (shutdown/recovery gate), metrics labels, and the OpenAPI-diff test.
