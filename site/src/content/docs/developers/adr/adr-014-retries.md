---
title: "ADR-014: The operation engine is the only retry authority"
description: "The operation engine is the only retry authority."
sidebar:
  label: "014 · The operation engine is the only retry authority"
---

Status: Accepted (2026-08-13)

## Decision

All retries of provider mutations are scheduled by the operation engine from **persisted** journal state (`attempt`, `next_attempt_at`) — restart-stable pacing (invariant 7). Defaults: base 2s, factor 2, cap 5m, full jitter; `Retry-After` taken as `max(computed, retryAfter)`; rate-limit errors additionally penalize the per-instance provider gate.

HTTP-client-level retries are forbidden: hcloud-go's built-in retries (ON by default since v2.11.0) are disabled with `WithRetryOpts{MaxRetries: 0}` — a hidden SDK retry of a create would bypass journal accounting. Config `providers.<name>.retry.*` parameterizes the engine's backoff, never the HTTP client.

Uncertain-create resolution: discovery by the `fleetplane.io/op` label within `provider.verifyWindow` (default 120s; per-driver override; calibrated against measured Hetzner list consistency in I10) before exactly one re-dispatch with the same operation ID.
