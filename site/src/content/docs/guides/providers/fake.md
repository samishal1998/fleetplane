---
title: "Fake provider"
description: "The deterministic in-memory driver for trying Fleetplane and for tests."
---

The `fake` driver is a deterministic in-memory provider — ideal for trying Fleetplane without a cloud account (the [quickstart](/fleetplane/guides/quickstart/) uses it) and the reference implementation every kernel test runs against.

An in-memory provider supporting both `compute.machine` and
`storage.volume`, used by tests and conformance suites. No credentials. The
zero-value settings are fully synchronous (creates land running
immediately, lists see everything at once); the knobs introduce
deterministic asynchrony:

| Setting | Type | Default | Behavior |
|---|---|---|---|
| `createSteps` | int | `0` | Observe calls until a create lands `running`. |
| `deleteSteps` | int | `0` | Observe calls until a delete lands gone. |
| `listLagSteps` | int | `0` | Discover calls before a new object becomes listable. |
| `pageSize` | int | `0` | Internal Discover pagination chunk (`0` = single page). |
| `park` | bool | `false` | Declares the park capability for `compute.machine` — the test control for [parked machines](/fleetplane/guides/concepts/#parked-machines-the-warm-tier). |
| `stopSteps` | int | `0` | Observe calls until a stop lands `stopped`. |
| `startSteps` | int | `0` | Observe calls until a start lands `running`. |
| `startEstimate` | duration | `0` | The `StartEstimate` hint the driver's park policy declares (feeds queue-wait estimates until observed data exists). |

```yaml
providers:
  local:
    driver: fake
    settings: {}
```

To see how the fake passes the provider conformance catalogue under its most hostile settings, see [writing a provider → the conformance suite](/fleetplane/developers/provider-sdk/#the-conformance-suite).
