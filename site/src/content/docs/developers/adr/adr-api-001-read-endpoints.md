---
title: "ADR-API-001: Additive read endpoints beyond doc 04 §3"
description: "Additive read endpoints beyond doc 04 §3."
sidebar:
  label: "API-001 · Additive read endpoints beyond doc 04 §3"
---

Status: Accepted (2026-08-13)

## Decision

The doc 08 §7 demo (`watch`, `pools`, `providers`) and operability need reads the doc 04 §3 table omits. Added (all additive, read-only except `:resolve`):

- `GET /v1/acquisitions/{id}` — `fleetplane watch` polls acquisition state.
- `GET /v1/pools`, `GET /v1/pools/{id}` — pool inspection.
- `GET /v1/operations` — operation listing (doc had only `GET /v1/operations/{id}`).
- `GET /v1/providers` — provider health surfacing (doc 07 §8: degraded providers surface separately from readiness).
- `POST /v1/operations/{id}:resolve` (permission `provider.admin`; actions `retry-verification` | `mark-failed`) — makes `uncertain` operations operable instead of frozen forever.
