# Cost-Aware Leasing and Billing Windows

## 1. Purpose

Fleetplane leases and infrastructure resources have **different lifetimes**.

A caller may request a resource for:

- 5 minutes
- 10 minutes
- 30 minutes
- 1 hour
- an unspecified duration

However, the underlying provider may bill that resource using a completely different billing model.

For example, a provider may have a minimum billable period or billing increment. A short-lived resource can therefore remain economically useful after its current lease ends because Fleetplane has already paid for the surrounding billing window.

Fleetplane should understand these billing characteristics and use them when scheduling leases, reusing resources, queueing requests, and deciding when to terminate resources.

This behavior is **provider-dependent**. Providers with sufficiently fine-grained billing do not need aggressive billing-window optimization.

---

## 2. Core Principle

Fleetplane must separate three concepts:

```text
Lease Lifetime
Resource Lifetime
Billing Lifetime
```

They are related, but they are not equivalent.

A lease represents how long a consumer requires capacity.

A resource represents the infrastructure object providing that capacity.

A billing window represents the period for which the provider charges Fleetplane.

For example:

```text
Resource created:       12:00
Provider billing window: 60 minutes

Lease A: 12:00 -> 12:10
Lease B: 12:15 -> 12:25

Resource:
12:00 ----------------------------------------> ~12:55
       AAAAAA      BBBBBB
```

Destroying the resource at 12:10 after Lease A would provide little or no cost benefit when the entire first hour is already billable.

Keeping it available allows Lease B to reuse already-paid capacity.

---

## 3. Provider Billing Model

Billing behavior belongs to the provider/resource implementation rather than the Fleetplane core.

A provider may expose billing characteristics similar to:

```go
type BillingPolicy struct {
    MinimumDuration    time.Duration
    BillingIncrement   time.Duration
    TerminationBuffer  time.Duration
}
```

Conceptually:

```yaml
billing:
  minimumDuration: 1h
  increment: 1h
  terminationBuffer: 5m
```

The exact SDK representation should be determined during implementation.

The important architectural requirement is that the scheduler can discover the billing characteristics of a resource.

---

## 4. Billing Policies Are Optional

Cost-aware billing-window behavior must **not** be assumed for every provider.

Providers with coarse billing increments or minimum charge periods benefit significantly from this optimization.

Providers with fine-grained billing may effectively expose:

```yaml
billing:
  increment: 1s
```

In that case Fleetplane should normally reclaim idle resources based on ordinary lease and idle policies rather than attempting to pack work into billing windows.

Therefore:

```text
Billing-aware scheduling is a capability/policy,
not a universal leasing rule.
```

The provider describes the billing model.

The Fleetplane scheduler decides how to use that information according to pool policy.

---

## 5. Fractional Leasing

Lease duration must not be constrained by provider billing duration.

A resource billed in one-hour increments can still accept:

```text
5 minute lease
10 minute lease
23 minute lease
45 minute lease
```

For example:

```text
Provider billing increment: 60 minutes

Machine created: 14:00

Lease A:
14:00 -> 14:10

Remaining paid window:
~50 minutes
```

Fleetplane should not destroy the machine simply because Lease A ended.

Instead, the machine returns to the available pool and may satisfy another acquisition.

---

## 6. Paid Capacity Reuse

When choosing between:

```text
A. reuse an existing idle resource
B. create a new resource
```

Fleetplane should prefer an existing compatible resource when doing so uses already-paid capacity and does not violate scheduling constraints.

Example:

```text
Machine A
Created: 10:00
Billing boundary: 11:00

Lease A:
10:00 -> 10:10

New request arrives:
10:20
Duration: 10 minutes
```

The scheduler should strongly prefer Machine A over creating Machine B.

---

## 7. Sequential Lease Packing

Billing-aware providers create another optimization opportunity: **queueing**.

Suppose two requests arrive simultaneously:

```text
Request A: 5 minutes
Request B: 5 minutes
```

Creating two machines may result in:

```text
Machine A: 1 billed hour
Machine B: 1 billed hour

Total billed capacity:
2 hours
```

But if Request B is allowed to wait:

```text
Machine A

00:00 -> 00:05  Request A
00:05 -> 00:10  Request B
```

both requests can potentially fit into the same billing window.

Fleetplane should therefore support a configurable queueing threshold.

---

## 8. Queueing Policy

Pools should be able to choose between cost and latency.

Conceptually:

```yaml
scheduling:
  queue:
    maxWait: 10m
```

When capacity is unavailable, Fleetplane can estimate:

```text
time until compatible capacity becomes available
```

If:

```text
estimated_wait <= max_wait
```

Fleetplane may queue the acquisition.

Otherwise it should provision additional capacity if allowed.

This allows different workloads to make different tradeoffs.

For example:

### Cost-oriented CI pool

```yaml
scheduling:
  queue:
    maxWait: 10m
```

### Latency-oriented production pool

```yaml
scheduling:
  queue:
    maxWait: 0s
```

The second configuration effectively disables queue-based packing.

---

## 9. Billing-Aware Placement

Candidate scoring should account for the remaining paid lifetime of existing resources.

Conceptually:

```text
score(resource) =
    compatibility
  + capacity_fit
  + paid_window_value
  - queue_delay
  - lifecycle_risk
```

An idle compatible resource with 35 minutes remaining in its current billing window should generally rank above provisioning another equivalent resource.

The exact scoring algorithm should remain replaceable.

---

## 10. Billing Boundaries

Fleetplane should calculate the next economically significant billing boundary.

For a resource created at:

```text
12:07
```

with:

```text
billing increment = 1 hour
```

the relevant boundaries are conceptually:

```text
13:07
14:07
15:07
...
```

Fleetplane should avoid accidentally crossing a billing boundary merely because resource termination takes time.

---

## 11. Termination Buffer

Resource deletion is not instantaneous.

For example:

```text
Billing boundary: 13:00
Delete requested: 12:59
Provider finishes deletion: 13:02
```

Depending on provider billing semantics, Fleetplane may now incur another billing increment.

Therefore Fleetplane needs a **termination buffer**.

Example:

```text
Billing boundary:     13:00
Termination buffer:   5 minutes
Safe termination:     12:55
```

Fleetplane should begin shutdown sufficiently early to achieve a high probability that provider-side termination completes before the billing boundary.

---

## 12. Adaptive Termination Buffer

A static buffer should be supported initially.

However, Fleetplane should collect:

```text
delete_requested_at
provider_confirmed_deleted_at
termination_duration
```

This allows future versions to calculate a provider-specific safety margin.

For example:

```text
observed p95 deletion duration = 2m 14s
configured safety margin       = 1m

termination buffer             = ~3m 14s
```

The control plane can therefore become more accurate over time.

A minimum configured buffer should still be available as a safety floor.

---

## 13. Termination Decision

An idle resource approaching a billing boundary should be evaluated approximately as:

```text
if active_leases:
    keep resource

else if queued_compatible_work_can_fit:
    keep resource

else if pool_requires_warm_capacity:
    keep resource

else if now >= next_billing_boundary - termination_buffer:
    terminate resource

else:
    keep resource available
```

This logic belongs in lifecycle policy rather than directly inside provider implementations.

Providers expose billing facts.

Fleetplane determines scheduling behavior.

---

## 14. Crossing a Billing Boundary Intentionally

Crossing a boundary is not necessarily wrong.

Suppose:

```text
Current time:       12:54
Billing boundary:   13:00

Queued lease:
duration = 30 minutes
```

Fleetplane may choose between:

```text
A. terminate the machine and create another later
B. retain the machine and intentionally enter the next billing window
```

If another hour will be billed either way, retaining the existing machine may be preferable because it avoids:

- provisioning latency;
- image boot time;
- provider API operations;
- additional failure opportunities.

Therefore the scheduler should reason about **incremental cost**, not merely whether a billing boundary exists.

---

## 15. Relationship to Autoscaling

Traditional autoscaling often models:

```text
demand -> required capacity
```

Fleetplane should additionally model:

```text
demand
+ queued demand
+ available capacity
+ already-paid capacity
+ upcoming billing boundaries
+ provisioning latency
+ termination latency
-> required capacity
```

This makes scaling decisions economically aware without coupling the core to a particular provider.

---

## 16. Example

Assume:

```text
Billing increment: 60 minutes
Termination buffer: 5 minutes
Queue max wait: 10 minutes
```

At 10:00:

```text
Request A arrives
duration: 10 minutes
```

Fleetplane creates Machine A.

At 10:10:

```text
Request A completes.
```

Machine A becomes idle but remains available.

At 10:18:

```text
Request B arrives
duration: 15 minutes
```

Machine A is reused.

At 10:33:

```text
Request B completes.
```

Machine A remains idle.

At approximately 10:55 Fleetplane evaluates the upcoming billing boundary.

If there is:

```text
no active lease
no acceptable queued request
no warm-capacity requirement
```

Fleetplane begins termination.

The goal is for provider-confirmed deletion to occur before approximately 11:00.

---

## 17. Scheduler Invariants

The implementation should preserve the following:

1. **Lease duration is independent of billing granularity.**
2. Ending a lease does not necessarily imply destroying its resource.
3. Compatible already-paid capacity should normally be preferred over creating equivalent new capacity.
4. Queueing must never exceed the acquisition's configured maximum wait.
5. Active leases prevent normal cost-based reclamation.
6. Pending/queued work must be considered before terminating reusable capacity.
7. Termination decisions must account for expected deletion duration.
8. Crossing a billing boundary may be intentional when doing so is cheaper or operationally preferable.
9. Billing optimization is provider/capability dependent.
10. Correctness and lease guarantees take precedence over cost optimization.

---

## 18. Required Metrics

Fleetplane should expose enough information to evaluate whether the optimization actually works:

```text
fleetplane_resource_paid_idle_seconds
fleetplane_resource_useful_seconds
fleetplane_resource_termination_seconds
fleetplane_billing_boundary_overruns_total
fleetplane_acquisition_queue_seconds
fleetplane_resource_reuse_total
fleetplane_scale_up_avoided_total
```

Later, if provider pricing information is available:

```text
fleetplane_estimated_cost
fleetplane_estimated_cost_saved
fleetplane_billed_unused_seconds
```

---

## 19. Implementation Sequence

### Phase 1

Implement:

- provider billing metadata;
- minimum duration;
- billing increment;
- static termination buffer;
- fractional leases;
- reuse after lease release;
- billing-boundary-aware termination.

### Phase 2

Add:

- acquisition queue;
- `maxWait`;
- sequential lease packing;
- billing-aware candidate scoring.

### Phase 3

Add:

- observed termination-duration metrics;
- adaptive termination buffer;
- incremental-cost estimation;
- cost-aware multi-provider placement.

---

## 20. Architectural Boundary

The most important boundary is:

```text
Provider:
"What are the billing and lifecycle characteristics of this resource?"

Fleetplane:
"Given those characteristics, current demand, leases, queue,
and policy, what is the economically sensible action?"
```

The provider should **not** decide when Fleetplane should keep a machine alive.

The scheduler should **not** contain hard-coded knowledge such as:

```text
if provider == "hetzner" { ... }
```

Instead, billing characteristics become capabilities/data exposed by providers and consumed by generic scheduling and lifecycle policies.

This keeps cost-aware leasing useful for providers with coarse billing behavior while allowing providers with fine-grained billing to use the ordinary leasing and reclamation path.