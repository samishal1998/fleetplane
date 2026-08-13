# ADR-006: cobra CLI as a pure HTTP client

Status: Accepted (2026-08-13)

## Decision

`github.com/spf13/cobra` for the CLI, sharing the `fleetplane` binary. Client command packages speak only to the public HTTP API (via `pkg/apiclient`) — never importing kernel packages — so the Phase-6 exit ("external automation needs no Go imports") holds by construction and is enforced by a `go list -deps` check.

Documented exception: `fleetplane admin backup` calls the ops listener's `/admin/backup` endpoint (it does not open the DB file directly).

Exit codes: 0 ok · 1 runtime · 2 usage · 3 not found · 4 auth · 5 conflict/idempotency · 6 watch timeout/acquisition failed · 7 server error.
