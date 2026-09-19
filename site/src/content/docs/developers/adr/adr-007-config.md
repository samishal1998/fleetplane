---
title: "ADR-007: YAML config via goccy/go-yaml + `secret://` references"
description: "YAML config via goccy/go-yaml + `secret://` references."
sidebar:
  label: "007 · YAML config via goccy/go-yaml + `secret://` references"
---

Status: Accepted (2026-08-13)

## Decision

Config is YAML decoded strictly with `github.com/goccy/go-yaml` (`gopkg.in/yaml.v3` was archived in April 2025). Unknown fields are boot errors with line/column. All cross-references (class → provider, pool → class, permission names, token digests) validate at boot; fail fast.

Credentials appear only as references — `secret://env/NAME`, `secret://file/path` — resolved at provider construction by `pkg/sdk/secretref` (scheme-extensible). Resolved values are never persisted, never logged (self-redacting `Secret` type), never returned by the API (doc 07 §4).
