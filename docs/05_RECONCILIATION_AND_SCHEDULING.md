# Reconciliation, Scheduling and Autoscaling

## 1. Reconciliation equation

For a desired pool:

```text
delta = desired_matching_resources - observed_matching_resources
```

But real reconciliation must account for:

- resources still provisioning;
- draining resources;
- failed resources;
- allocations;
- cooldowns;
- provider quota;
- pending create/delete operations;
- adoption policy;
- min-ready requirements.

## 2. Reconciler responsibilities

- refresh observations;
- detect drift;
- calculate desired changes;
- create an explicit plan;
- persist the plan/operation;
- execute bounded mutations;
- retry transient failures;
- re-observe;
- converge.

## 3. Scheduler responsibilities

When acquiring capacity:

1. filter by resource kind;
2. filter by provider/class/labels;
3. filter by hard constraints;
4. exclude unhealthy/draining resources;
5. calculate available capacity;
6. score candidates;
7. atomically reserve capacity;
8. if no candidate exists, request scale-up if allowed.

## 4. Scoring

v1 can use deterministic first-fit/best-fit. Later scoring plugins can consider:

- packing efficiency;
- cost;
- startup latency;
- location;
- provider;
- spot interruption risk;
- data locality.

## 5. Scale-up

A scale-up should be represented as an operation before provider mutation.

```text
Acquire request
 -> no capacity
 -> create reservation/pending acquisition
 -> planner chooses class/provider
 -> journal CreateResource action
 -> provider creates VM
 -> poll until ready
 -> observe
 -> bind acquisition
```

## 6. Scale-down

A resource becomes reclaimable when policy says it is safe:

```text
no active leases
AND not protected
AND idle duration >= threshold
AND pool remains >= minimum/warm capacity
```

Scale-down uses `draining -> deleting -> deleted`, not an immediate untracked delete.

## 7. Failure states

Suggested phases:

```text
unknown
provisioning
ready
allocated
draining
deleting
failed
orphaned
```

These are orchestration phases, not provider-native machine states.

## 8. Reconciliation triggers

- periodic timer;
- API mutation;
- acquisition/release;
- operation completion;
- provider error recovery;
- manual reconcile.

No external webhook is required for correctness.

## 9. Rate limiting

Each provider instance should have:

- concurrency limit;
- token-bucket/request-rate limit;
- exponential backoff with jitter;
- Retry-After support;
- circuit-breaker/health state where useful.

## 10. Eventually consistent providers

Create success does not imply immediate discoverability. The operation engine must preserve the returned external reference and poll using provider semantics rather than assuming a list call immediately contains the resource.
