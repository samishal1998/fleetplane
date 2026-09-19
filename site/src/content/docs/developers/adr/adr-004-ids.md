---
title: "ADR-004: Prefixed ULIDs for all entity IDs"
description: "Prefixed ULIDs for all entity IDs."
sidebar:
  label: "004 · Prefixed ULIDs for all entity IDs"
---

Status: Accepted (2026-08-13)

## Decision

IDs are `<prefix>_<ULID>` via `github.com/oklog/ulid/v2`: `res_`, `pool_`, `acq_`, `lease_`, `op_`, `evt_`, `tok_`, `req_`. Time-ordered and lexicographically sortable (free creation-order scans on TEXT primary keys; deterministic scheduler tie-breaks); prefixes make IDs self-describing in logs and audit records.

Events use the `evt_` ULID as both primary key and pagination cursor (no rowid cursors). The prefix table lives in `internal/ids` — the single source of truth.
