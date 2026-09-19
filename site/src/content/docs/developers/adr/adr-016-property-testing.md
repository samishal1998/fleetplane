---
title: "ADR-016: Property-based testing with pgregory.net/rapid"
description: "Property-based testing with pgregory.net/rapid."
sidebar:
  label: "016 · Property-based testing with pgregory.net/rapid"
---

Status: Accepted (2026-08-13)

## Decision

`pgregory.net/rapid` for property tests (Hypothesis-style shrinking; `rapid.StateMachine` fits the journal/reconciliation invariants). Core properties: idempotency-replay-is-noop, reconcile-converges-under-random-faults, capacity-never-oversubscribed, journal state machine with crash actions.

License note: rapid is MPL-2.0 (file-level copyleft). It is a **test-only** dependency, never linked into the shipped binary — acceptable; flagged here for any future legal review. Failing seeds are committed as regression files.
