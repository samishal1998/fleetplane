# ADR-018: Cost-aware leasing and billing windows

Status: accepted (implements docs/11; design adversarially verified, 29
findings reconciled — 4 blockers reshaped the design as noted below).

## Decisions

1. **Billing facts are a capability, policy is kernel-side** (docs/11 §20).
   `provider.BillingPolicy{MinimumDuration, BillingIncrement,
   TerminationBuffer}` with the zero value meaning fine-grained (no
   billing-window behavior). Drivers opt in via the `provider.BillingAware`
   type-asserted interface (ADR-013 style). Config overrides per
   instance+kind (`providers.<n>.billing.<kind>`) merge **field-wise over
   pointer fields** — a partial override never silently zeroes the rest —
   with `disabled: true` as the explicit opt-out. Override kinds are
   validated against the driver's declared kinds at boot.

2. **One canonical `paid(t)`** (internal/billing): `increment > 0 →
   max(min, ceil(t/inc)·inc)`; `increment = 0 → max(min, t)`. Billing
   boundaries are its discontinuities; the §18 accounting uses the same
   function — the two can never disagree (finding: min-not-multiple-of-
   increment produced boundaries matching no billing semantics).

3. **Termination is a window, not a half-line**: delete only inside
   `[boundary − buffer, boundary − cutoff]` with `cutoff = buffer/2`. Past
   the cutoff the increment is inescapable → keep and target the next
   boundary (docs/11 §14 becomes an explicit decision;
   `fleetplane_billing_window_missed_total` counts these). The effective
   buffer has a kernel floor of `2·sweepInterval + 5s` — a zero policy
   buffer would otherwise make the window unhittable and strand resources
   forever (blocker). Optional adaptive buffer: p95 of recent
   first-attempt delete durations + 60s, capped at increment/2.

4. **Cost reclaim is one transaction** (blocker): the poolless idle sweep
   journals the delete directly from `Ready` with every gate inside the tx
   (phase, ownership, protection, leases, queued-work). No parking in
   `Draining` — a parked resource is invisible to the scheduler while the
   queued acquisition that blocks its deletion waits for it (deadlock).
   Boundary delay applies **only** to the poolless class-reclaim sweep;
   pool surplus draining converges to a declared replica count (explicit
   intent) and keeps today's timing (blocker: the two were one code path).
   Cost deletes have their own per-sweep bound instead of competing with
   pool convergence for `maxMutationsPerCycle`.

5. **Queueing reuses `pending`**: the effective `maxWait` (request value,
   else `classes.<n>.scheduling.queue.maxWait`) is resolved at ACCEPT time,
   clamped to `pendingTimeout − 10s` (clamped, never rejected — rejection
   against a config-dependent limit would break byte-identical idempotent
   replay), and persisted inside the acquisition's request payload
   (`capacity.Request.maxWaitMs`) — no schema migration, and a restart
   resumes exactly the same deadline. Deadline force-scale runs BEFORE the
   pendingTimeout expiry check in the sweep, and a provisioning
   acquisition's expiry clock restarts at the scale transition.

6. **Wait estimation** (v1): min over compatible resources of (a) latest
   expiry of a leased resource's blocking leases (all must carry TTLs), or
   (b) for an in-flight provisioning resource of the same class: observed
   p50 create duration + the TTL of the acquisition pre-bound to it — this
   is what makes docs/11 §7's simultaneous-request packing reachable
   (blocker: provisioning resources were invisible to estimation). No
   estimate → scale, the latency-safe default.

7. **Queued work protects only what it can use**: the reclaim gate matches
   queued acquisitions to resources with the scheduler's own compatibility
   predicate (kind, class, capacity fit) and greedy counting — one queued
   request protects one best-fit machine, never the whole class.

8. **Placement** (docs/11 §9): candidates whose remaining window covers the
   lease TTL **before the safe-termination point** rank first (smallest
   slack); fits-before-boundary-but-inside-buffer ranks second; capacity
   best-fit + ULID break ties. One replaceable function.

9. **Anchor approximation**: boundaries anchor on the resource row's
   created_at (early ≈ safe: deletes dispatch early, never late); the
   buffer absorbs journal→provider-accept skew. Documented limitation.

10. **Deferred with seams**: currency cost estimation (providers expose no
    pricing), multi-provider cost placement, queue estimates for shared
    (non-exclusive) capacity fractions, pool-level billing windows.
