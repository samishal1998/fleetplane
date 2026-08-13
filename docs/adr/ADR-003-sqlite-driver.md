# ADR-003: modernc.org/sqlite as the storage driver

Status: Accepted (2026-08-13)

## Decision

`modernc.org/sqlite` (≥ v1.56, embeds SQLite 3.51.x): pure-Go, `database/sql`-compatible, supports `_pragma=`/`_txlock=immediate` DSN options. Rejected `mattn/go-sqlite3` (CGo breaks the static-binary goal) and `zombiezen.com/go/sqlite` (no `database/sql`; its perf edge is irrelevant at control-plane write volume, and `database/sql` keeps the doc-06 Postgres path cheap).

## Connection discipline

Two handles: a 1-connection writer (`_txlock=immediate`, `synchronous=FULL` — the operation journal must survive power loss) and an N-connection reader pool (`synchronous=NORMAL`), plus one dedicated backup connection for `VACUUM INTO`. WAL mode; PRAGMAs live only in the DSN (ADR-010).

## Risk

modernc is a C transpile; rare historical corruption bugs are mitigated by startup `integrity_check`, the crash-matrix CI, and the Store seam (driver swap without kernel change).
