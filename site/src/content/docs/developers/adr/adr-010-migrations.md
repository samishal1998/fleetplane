---
title: "ADR-010: goose migrations; PRAGMAs live in the DSN"
description: "goose migrations; PRAGMAs live in the DSN."
sidebar:
  label: "010 · goose migrations; PRAGMAs live in the DSN"
---

Status: Accepted (2026-08-13)

## Decision

`github.com/pressly/goose/v3` with SQL files embedded via `embed.FS` (`goose.SetBaseFS`, dialect `sqlite`). Per-migration transactions; forward-only in production. Startup refuses to run if the DB schema version exceeds the binary's known maximum, and a backup (ADR-003) is taken before migrating.

Connection PRAGMAs (`journal_mode=WAL`, `busy_timeout`, `foreign_keys`, `synchronous`) are set only via DSN `_pragma=` parameters — never inside migrations: `journal_mode` cannot change inside a transaction, and per-connection PRAGMAs don't persist from a migration anyway. Migrations are pure DDL/DML.

Rejected: golang-migrate (heavier, split up/down files) and an in-house runner (goose is verified working with modernc and already provides the version ledger + per-migration tx).
