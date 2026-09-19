---
title: "ADR-015: Hetzner E2E safety protocol"
description: "Hetzner E2E safety protocol."
sidebar:
  label: "015 · Hetzner E2E safety protocol"
---

Status: Accepted (2026-08-13)

## Rules

- Double gate: tests skip unless `FLEETPLANE_E2E=1` **and** `HETZNER_TOKEN` are set (prevents accidental dev-shell runs).
- Token belongs to a **dedicated throwaway Hetzner project** — never production.
- Every E2E resource is labeled `fleetplane.io/test=1` and `fleetplane.io/test-run=<run id>`.
- Cleanup: `t.Cleanup` per test, an `if: always()` sweeper step in CI (deletes this run's resources immediately; any test-labeled resource older than 2h as backstop), and a nightly scheduled sweep. The sweeper refuses to touch anything lacking the test label — conservative destruction applies to test tooling too.
- CI: E2E runs only on main pushes, nightly schedule, or manual dispatch (never fork PRs); a `preflight` job exposes `has_token` as an output because the `secrets` context is unavailable in job-level `if`; runs are serialized via `concurrency` with a 30-minute timeout.
