// Package scheduler satisfies acquisitions (docs/05 §3–5): filter → score
// (deterministic best-fit) → ATOMIC reservation, falling back to
// scale-on-demand with pre-binding. Concurrent acquires can never
// over-allocate: the capacity check and the lease insert commit atomically
// under the single-writer BEGIN IMMEDIATE transaction, an in-transaction
// re-aggregation asserts the invariant post-insert, and the exclusive
// partial unique index is an independent DB-level backstop (invariant 1).
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/samishal1998/fleetplane/internal/billing"
	"github.com/samishal1998/fleetplane/internal/capacity"
	"github.com/samishal1998/fleetplane/internal/ids"
	"github.com/samishal1998/fleetplane/internal/metrics"
	"github.com/samishal1998/fleetplane/internal/phase"
	"github.com/samishal1998/fleetplane/internal/provision"
	"github.com/samishal1998/fleetplane/internal/reconcile"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/pkg/sdk"
)

// ErrNoCapacity: no candidate fits and scale-on-demand is not possible.
var ErrNoCapacity = errors.New("no capacity available and no class to scale from")

type Kicker interface{ Kick() }

type Scheduler struct {
	st        storage.Store
	providers provision.Providers
	engine    Kicker
	classes   reconcile.ClassResolver
	clock     sdk.Clock
	log       *slog.Logger
	ownerID   string

	// queued tracks acquisitions currently held pending by the queue
	// policy (docs/11 §7–8) — in-memory only: after a restart, a pending
	// acquisition with maxWaitMs re-queues through the same decision, so
	// the flag reconstitutes itself (the deadline itself is persisted).
	qmu    sync.Mutex
	queued map[storage.AcquisitionID]bool

	// startFail is the per-resource start-failure backoff (docs/12 design
	// blocker): a parked machine whose start keeps failing must not be the
	// deterministic best-fit forever — rung 2 skips machines under backoff
	// so the ladder falls through to create. In-memory: a restart forgives
	// one extra attempt, bounded by the engine's own attempt cap.
	bmu       sync.Mutex
	startFail map[storage.ResourceID]startFailState
}

type startFailState struct {
	untilMs  int64
	attempts int
}

func New(st storage.Store, providers provision.Providers, engine Kicker,
	classes reconcile.ClassResolver, clock sdk.Clock, log *slog.Logger, ownerID string) *Scheduler {
	return &Scheduler{st: st, providers: providers, engine: engine, classes: classes,
		clock: clock, log: log, ownerID: ownerID,
		queued:    map[storage.AcquisitionID]bool{},
		startFail: map[storage.ResourceID]startFailState{}}
}

// Classes exposes the class resolver (shared with pool validation).
func (s *Scheduler) Classes() reconcile.ClassResolver { return s.classes }

// Satisfy tries to bind a pending acquisition: reuse existing capacity
// first (05 §3), otherwise plan a scale-on-demand create (05 §5).
func (s *Scheduler) Satisfy(ctx context.Context, acqID storage.AcquisitionID) (*storage.Acquisition, error) {
	acq, err := s.st.Acquisitions().Get(ctx, acqID)
	if err != nil {
		return nil, err
	}
	if acq.State != storage.AcqPending {
		return acq, nil // already satisfied / terminal
	}
	req, want, exclusive, err := capacity.ParseRequest(acq.Constraints)
	if err != nil {
		return nil, err
	}

	// Steps 1–6 (05 §3): filter + score on a read snapshot, no locks.
	candidates, err := s.rankedCandidates(ctx, acq, want, exclusive)
	if err != nil {
		return nil, err
	}
	// Step 7: atomic reservation, bounded attempts over ranked candidates.
	for i, cand := range candidates {
		if i >= 3 {
			break
		}
		lease, err := s.reserve(ctx, acq, cand.ID, want, exclusive)
		if err == nil {
			s.log.Info("acquisition bound to existing capacity",
				"acquisition_id", acq.ID, "resource_id", cand.ID, "lease_id", lease)
			s.observeBound(acq, cand.ID)
			return s.st.Acquisitions().Get(ctx, acqID)
		}
		if !errors.Is(err, storage.ErrConflict) && !errors.Is(err, errInsufficient) {
			return nil, err
		}
	}

	// Rung 2 (docs/12 §6): start a parked compatible machine — seconds
	// instead of minutes, on capacity that is already owned and nearly
	// free while stopped.
	if resumed, err := s.startParked(ctx, acq, want); err != nil {
		return nil, err
	} else if resumed {
		return s.st.Acquisitions().Get(ctx, acqID)
	}

	// Queue decision (docs/11 §7–8): with a queue contract and capacity
	// expected inside the deadline, stay pending instead of scaling. The
	// 5s acquisition sweep re-runs Satisfy; past the deadline this branch
	// no longer applies and the force-scale below happens naturally.
	if req.MaxWaitMs > 0 {
		nowMs := s.clock.Now().UnixMilli()
		deadline := acq.CreatedAt + req.MaxWaitMs
		if nowMs < deadline {
			if est, ok := s.estimateWait(ctx, acq, want); ok && nowMs+est.Milliseconds() <= deadline {
				s.markQueued(ctx, acq, est)
				return acq, nil // deliberately still pending
			}
		}
	}

	// Step 8: scale on demand (needs a class).
	return s.scaleOnDemand(ctx, acq)
}

// startParked claims the best-fit parked compatible machine for the
// acquisition: one transaction moves the acquisition to provisioning,
// journals the start (the parked→starting CAS serializes racing claimers)
// and pre-binds via pending_resource_id — the existing op-terminal bind
// path completes it (docs/12 §6).
func (s *Scheduler) startParked(ctx context.Context, acq *storage.Acquisition, want capacity.Vector) (bool, error) {
	list, err := s.st.Resources().List(ctx, storage.ResourceFilter{
		Kind:      acq.Kind,
		Class:     acq.Class,
		Phases:    []phase.Phase{phase.Parked},
		Ownership: []storage.Ownership{storage.OwnershipManaged},
	})
	if err != nil {
		return false, err
	}
	nowMs := s.clock.Now().UnixMilli()
	type cand struct {
		id    storage.ResourceID
		score int64
	}
	var cands []cand
	for _, res := range list {
		if s.underStartBackoff(res.ID, nowMs) {
			continue
		}
		if !s.providers.Parking(res.Provider, res.Kind).Supported {
			continue
		}
		total, err := capacity.Parse(res.Capacity)
		if err != nil || !capacity.Fits(total, capacity.Vector{}, want) {
			continue
		}
		cands = append(cands, cand{id: res.ID, score: capacity.FreeAfter(total, capacity.Vector{}, want)})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score < cands[j].score
		}
		return cands[i].id < cands[j].id
	})
	for i, c := range cands {
		if i >= 3 {
			break
		}
		err := s.st.Tx(ctx, func(tx storage.TxStore) error {
			if err := tx.Acquisitions().Transition(ctx, acq.ID, storage.AcqPending, storage.AcqProvisioning, nowMs); err != nil {
				return err
			}
			if _, err := provision.JournalStart(ctx, tx, s.providers, c.id, nowMs, acq.Actor); err != nil {
				return err
			}
			return tx.Acquisitions().SetPendingResource(ctx, acq.ID, c.id)
		})
		if err == nil {
			s.engine.Kick()
			s.log.Info("acquisition starting a parked machine (docs/12)",
				"acquisition_id", acq.ID, "resource_id", c.id)
			return true, nil
		}
		if !errors.Is(err, storage.ErrConflict) {
			return false, err
		}
		// CAS lost (another claimer) — try the next candidate.
	}
	return false, nil
}

func (s *Scheduler) underStartBackoff(id storage.ResourceID, nowMs int64) bool {
	s.bmu.Lock()
	defer s.bmu.Unlock()
	return nowMs < s.startFail[id].untilMs
}

func (s *Scheduler) bumpStartBackoff(id storage.ResourceID, nowMs int64) {
	s.bmu.Lock()
	defer s.bmu.Unlock()
	st := s.startFail[id]
	st.attempts++
	delay := (30 * time.Second) << min(st.attempts-1, 5) // 30s..16m
	if delay > 10*time.Minute {
		delay = 10 * time.Minute
	}
	st.untilMs = nowMs + delay.Milliseconds()
	s.startFail[id] = st
}

func (s *Scheduler) clearStartBackoff(id storage.ResourceID) {
	s.bmu.Lock()
	defer s.bmu.Unlock()
	delete(s.startFail, id)
}

// markQueued records the queue decision (idempotent; evented once).
func (s *Scheduler) markQueued(ctx context.Context, acq *storage.Acquisition, est time.Duration) {
	s.qmu.Lock()
	first := !s.queued[acq.ID]
	s.queued[acq.ID] = true
	s.qmu.Unlock()
	if !first {
		return
	}
	s.log.Info("acquisition queued for existing capacity (docs/11 §7)",
		"acquisition_id", acq.ID, "class", acq.Class, "estimated_wait", est.String())
	_ = s.st.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Events().Append(ctx, &storage.Event{
			TS: s.clock.Now().UnixMilli(), Actor: acq.Actor, Type: "acquisition.queued",
			Outcome: "queued",
			Details: json.RawMessage(fmt.Sprintf(`{"acquisitionId":%q,"estimatedWait":%q}`, acq.ID, est.String())),
		})
	})
}

// observeBound records reuse/queue metrics when an acquisition binds to
// EXISTING capacity (never for pre-bound fresh creates).
func (s *Scheduler) observeBound(acq *storage.Acquisition, resID storage.ResourceID) {
	if acq.PendingResourceID != nil && *acq.PendingResourceID == resID {
		return // fresh scale-on-demand create, not reuse
	}
	metrics.ResourceReuse.WithLabelValues(acq.Class).Inc()
	s.qmu.Lock()
	wasQueued := s.queued[acq.ID]
	delete(s.queued, acq.ID)
	s.qmu.Unlock()
	if wasQueued {
		metrics.ScaleUpAvoided.WithLabelValues(acq.Class).Inc()
		metrics.AcquisitionQueueSeconds.Observe(float64(s.clock.Now().UnixMilli()-acq.CreatedAt) / 1000)
	}
}

// estimateWait predicts when compatible capacity frees up (docs/11 §8, v1):
//   - a compatible leased resource whose blocking leases ALL carry expiries
//     frees at the latest of them;
//   - a compatible in-flight provisioning resource frees after the observed
//     provisioning time plus the TTL of the acquisition pre-bound to it
//     (nothing pre-bound: just the provisioning time) — this is what makes
//     §7's simultaneous-request packing reachable at all.
//
// No computable estimate => (0, false): the caller scales, the
// latency-safe default.
func (s *Scheduler) estimateWait(ctx context.Context, acq *storage.Acquisition, want capacity.Vector) (time.Duration, bool) {
	list, err := s.st.Resources().List(ctx, storage.ResourceFilter{
		Kind:      acq.Kind,
		Class:     acq.Class,
		Phases:    []phase.Phase{phase.Ready, phase.Allocated, phase.Provisioning, phase.Starting},
		Ownership: []storage.Ownership{storage.OwnershipManaged},
	})
	if err != nil {
		return 0, false
	}
	nowMs := s.clock.Now().UnixMilli()
	var provisioningAcqs []*storage.Acquisition
	best, found := time.Duration(0), false
	better := func(d time.Duration) {
		if d < 0 {
			d = 0
		}
		if !found || d < best {
			best, found = d, true
		}
	}
	for _, res := range list {
		if res.Phase == phase.Starting {
			// A machine being started for another acquisition frees after
			// the observed start latency plus that holder's TTL (docs/12).
			if acq.Class == "" || res.Class != acq.Class {
				continue
			}
			if provisioningAcqs == nil {
				provisioningAcqs, _ = s.st.Acquisitions().ListByState(ctx, storage.AcqProvisioning)
			}
			holdMs, skip := int64(0), false
			for _, pa := range provisioningAcqs {
				if pa.PendingResourceID != nil && *pa.PendingResourceID == res.ID {
					if pa.TTLSeconds <= 0 {
						skip = true
					}
					holdMs = pa.TTLSeconds * 1000
					break
				}
			}
			if skip {
				continue
			}
			better(s.startEstimate(ctx, res.Provider, res.Kind) + time.Duration(holdMs)*time.Millisecond)
			continue
		}
		if res.Phase == phase.Provisioning {
			// Capacity is unknown until the create lands; estimable only
			// for class-matched requests (the class fixes the shape).
			if acq.Class == "" || res.Class != acq.Class {
				continue
			}
			if provisioningAcqs == nil {
				provisioningAcqs, _ = s.st.Acquisitions().ListByState(ctx, storage.AcqProvisioning)
			}
			holdMs := int64(0)
			skip := false
			for _, pa := range provisioningAcqs {
				if pa.PendingResourceID != nil && *pa.PendingResourceID == res.ID {
					if pa.TTLSeconds <= 0 {
						skip = true // unbounded hold: no estimate from this one
					}
					holdMs = pa.TTLSeconds * 1000
					break
				}
			}
			if skip {
				continue
			}
			better(s.provisioningEstimate(ctx, res.Provider) + time.Duration(holdMs)*time.Millisecond)
			continue
		}
		total, err := capacity.Parse(res.Capacity)
		if err != nil || !capacity.Fits(total, capacity.Vector{}, want) {
			continue // can never host this request
		}
		active, err := s.st.Leases().ActiveByResource(ctx, res.ID)
		if err != nil || len(active) == 0 {
			continue // idle-but-unreservable was already tried above
		}
		freeAt, ok := int64(0), true
		for _, l := range active {
			if l.ExpiresAt == nil {
				ok = false // a lease with no TTL: unknown wait
				break
			}
			if *l.ExpiresAt > freeAt {
				freeAt = *l.ExpiresAt
			}
		}
		if ok {
			better(time.Duration(freeAt-nowMs) * time.Millisecond)
		}
	}
	return best, found
}

// startEstimate is the p50 of recent observed start durations, falling
// back to the driver's hint, then 60s (docs/12 §6).
func (s *Scheduler) startEstimate(ctx context.Context, prov storage.ProviderInstance, kind string) time.Duration {
	ops, err := s.st.Operations().RecentTerminal(ctx, prov, storage.OpKindStart, 20)
	if err == nil && len(ops) > 0 {
		ds := make([]time.Duration, 0, len(ops))
		for _, op := range ops {
			ds = append(ds, time.Duration(op.UpdatedAt-op.CreatedAt)*time.Millisecond)
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds[len(ds)/2]
	}
	if hint := s.providers.Parking(prov, kind).StartEstimate; hint > 0 {
		return hint
	}
	return 60 * time.Second
}

// provisioningEstimate is the p50 of recent observed create durations for
// the provider, defaulting to 90s with no samples.
func (s *Scheduler) provisioningEstimate(ctx context.Context, prov storage.ProviderInstance) time.Duration {
	ops, err := s.st.Operations().RecentTerminal(ctx, prov, storage.OpKindCreate, 20)
	if err != nil || len(ops) == 0 {
		return 90 * time.Second
	}
	ds := make([]time.Duration, 0, len(ops))
	for _, op := range ops {
		ds = append(ds, time.Duration(op.UpdatedAt-op.CreatedAt)*time.Millisecond)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

type candidate struct {
	ID    storage.ResourceID
	score int64
	// Billing tier (docs/11 §9, design-verified against the buffer):
	//   0 = lease TTL fits before the SAFE termination point (boundary -
	//       buffer) — reclaim stays possible; ranked by smallest slack;
	//   1 = fits before the boundary but inside the buffer — placing here
	//       defers this machine's clean reclaim;
	//   2 = no TTL / fine-grained billing / crosses the boundary.
	tier  int
	slack int64
}

func (s *Scheduler) rankedCandidates(ctx context.Context, acq *storage.Acquisition, want capacity.Vector, exclusive bool) ([]candidate, error) {
	list, err := s.st.Resources().List(ctx, storage.ResourceFilter{
		Kind:      acq.Kind,
		Class:     acq.Class,
		Phases:    []phase.Phase{phase.Ready, phase.Allocated},
		Ownership: []storage.Ownership{storage.OwnershipManaged},
	})
	if err != nil {
		return nil, err
	}
	var out []candidate
	for _, res := range list {
		if res.DeleteProtected && exclusive {
			// protected resources may still be leased; protection only
			// guards deletion — no filter here.
			_ = res
		}
		total, err := capacity.Parse(res.Capacity)
		if err != nil {
			continue
		}
		active, err := s.st.Leases().ActiveByResource(ctx, res.ID)
		if err != nil {
			return nil, err
		}
		if exclusive && len(active) > 0 {
			continue
		}
		blocked := false
		used := capacity.Vector{}
		for _, l := range active {
			if l.Exclusive {
				blocked = true // an exclusive holder blocks shared joiners
				break
			}
			lc, _ := capacity.Parse(l.Capacity)
			for k, v := range lc {
				used[k] += v
			}
		}
		if blocked {
			continue
		}
		if !capacity.Fits(total, used, want) {
			continue
		}
		c := candidate{ID: res.ID, score: capacity.FreeAfter(total, used, want), tier: 2}
		if acq.TTLSeconds > 0 {
			if pol, _ := s.providers.Billing(res.Provider, res.Kind); !pol.FineGrained() {
				nowMs := s.clock.Now().UnixMilli()
				if b, ok := billing.NextBoundary(res.CreatedAt, pol, nowMs); ok {
					// The scoring floor mirrors the reclaim sweep's buffer
					// closely enough for placement (15s = default interval).
					buffer := billing.EffectiveBuffer(pol, 15*time.Second)
					leaseEnd := nowMs + acq.TTLSeconds*1000
					safe := b - buffer.Milliseconds()
					switch {
					case leaseEnd <= safe:
						c.tier, c.slack = 0, safe-leaseEnd
					case leaseEnd <= b:
						c.tier = 1
					}
				}
			}
		}
		out = append(out, c)
	}
	// Deterministic order: billing tier, then paid-window best-fit (tier
	// 0), then capacity best-fit, ULID tie-break (docs/11 §9 — the scoring
	// stays one replaceable function).
	sort.Slice(out, func(i, j int) bool {
		if out[i].tier != out[j].tier {
			return out[i].tier < out[j].tier
		}
		if out[i].tier == 0 && out[i].slack != out[j].slack {
			return out[i].slack < out[j].slack
		}
		if out[i].score != out[j].score {
			return out[i].score < out[j].score
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

var errInsufficient = errors.New("scheduler: insufficient capacity")

// reserve is the reservation transaction (05 §3 step 7): everything is
// re-read and re-checked inside the single-writer tx.
func (s *Scheduler) reserve(ctx context.Context, acq *storage.Acquisition, resID storage.ResourceID, want capacity.Vector, exclusive bool) (storage.LeaseID, error) {
	leaseID := storage.LeaseID(ids.New(ids.Lease))
	nowMs := s.clock.Now().UnixMilli()
	err := s.st.Tx(ctx, func(tx storage.TxStore) error {
		res, err := tx.Resources().Get(ctx, resID)
		if err != nil {
			return err
		}
		if res.DeletedAt != nil || (res.Phase != phase.Ready && res.Phase != phase.Allocated) ||
			res.Ownership != storage.OwnershipManaged {
			return fmt.Errorf("%w: candidate no longer eligible", storage.ErrConflict)
		}
		total, err := capacity.Parse(res.Capacity)
		if err != nil {
			return err
		}
		active, err := tx.Leases().ActiveByResource(ctx, resID)
		if err != nil {
			return err
		}
		if exclusive && len(active) > 0 {
			return errInsufficient
		}
		used := capacity.Vector{}
		for _, l := range active {
			if l.Exclusive {
				return errInsufficient
			}
			lc, _ := capacity.Parse(l.Capacity)
			for k, v := range lc {
				used[k] += v
			}
		}
		if !capacity.Fits(total, used, want) {
			return errInsufficient
		}

		capJSON, _ := json.Marshal(want)
		var expires *int64
		if acq.TTLSeconds > 0 {
			e := nowMs + acq.TTLSeconds*1000
			expires = &e
		}
		if err := tx.Leases().Insert(ctx, &storage.Lease{
			ID: leaseID, AcquisitionID: acq.ID, ResourceID: resID,
			Holder: acq.Actor, Capacity: capJSON, Exclusive: exclusive,
			State: storage.LeaseActive, ExpiresAt: expires, CreatedAt: nowMs,
		}); err != nil {
			return err
		}
		// Belt and braces: re-aggregate post-insert (invariant 1).
		sum, err := tx.Leases().SumActive(ctx, resID)
		if err != nil {
			return err
		}
		for dim, t := range total {
			if sum[dim] > t {
				return fmt.Errorf("%w: post-insert over-allocation on %s", storage.ErrConflict, dim)
			}
		}
		if res.Phase == phase.Ready {
			if err := tx.Resources().CASPhase(ctx, resID, phase.Ready, phase.Allocated, nowMs); err != nil {
				return err
			}
		}
		if err := tx.Acquisitions().Bind(ctx, acq.ID, leaseID, nowMs); err != nil {
			return err
		}
		return tx.Events().Append(ctx, &storage.Event{
			TS: nowMs, Actor: acq.Actor, Type: "acquisition.bound",
			ResourceID: &resID, Provider: "", Outcome: "bound",
			Details: json.RawMessage(fmt.Sprintf(`{"acquisitionId":%q,"leaseId":%q}`, acq.ID, leaseID)),
		})
	})
	if err != nil {
		return "", err
	}
	return leaseID, nil
}

// scaleOnDemand pre-binds a fresh create to the acquisition (05 §5): one
// transaction moves the acquisition to provisioning, journals the create
// and records pending_resource_id (invariant 4 by co-creation).
func (s *Scheduler) scaleOnDemand(ctx context.Context, acq *storage.Acquisition) (*storage.Acquisition, error) {
	if acq.Class == "" {
		return nil, ErrNoCapacity
	}
	cls, ok := s.classes.Class(acq.Class)
	if !ok {
		return nil, fmt.Errorf("unknown class %q", acq.Class)
	}
	nowMs := s.clock.Now().UnixMilli()
	prepared, err := provision.Prepare(ctx, s.providers, s.ownerID, provision.CreateSpec{
		Kind: cls.Kind, Provider: cls.Provider, Spec: cls.Spec, Class: acq.Class,
		Labels: map[string]string{"fleetplane.io/acquisition": string(acq.ID)},
	}, nowMs)
	if err != nil {
		return nil, err
	}
	err = s.st.Tx(ctx, func(tx storage.TxStore) error {
		if err := tx.Acquisitions().Transition(ctx, acq.ID, storage.AcqPending, storage.AcqProvisioning, nowMs); err != nil {
			return err
		}
		if err := prepared.Journal(ctx, tx, acq.Actor, ""); err != nil {
			return err
		}
		return tx.Acquisitions().SetPendingResource(ctx, acq.ID, prepared.Resource.ID)
	})
	if err != nil {
		return nil, err
	}
	s.engine.Kick()
	s.log.Info("acquisition provisioning new capacity",
		"acquisition_id", acq.ID, "resource_id", prepared.Resource.ID)
	return s.st.Acquisitions().Get(ctx, acq.ID)
}

// HandleOpTerminal binds waiting acquisitions when their pre-bound create
// OR start lands (wired into engine.OnTerminal alongside the reconciler).
func (s *Scheduler) HandleOpTerminal(op *storage.Operation) {
	if (op.Kind != storage.OpKindCreate && op.Kind != storage.OpKindStart) || op.ResourceID == nil {
		return
	}
	if op.Kind == storage.OpKindStart {
		switch op.State {
		case storage.OpSucceeded:
			s.clearStartBackoff(*op.ResourceID)
		case storage.OpFailed, storage.OpAborted:
			s.bumpStartBackoff(*op.ResourceID, s.clock.Now().UnixMilli())
		}
	}
	ctx := context.Background()
	open, err := s.st.Acquisitions().ListByState(ctx, storage.AcqProvisioning)
	if err != nil {
		return
	}
	for _, acq := range open {
		if acq.PendingResourceID == nil || *acq.PendingResourceID != *op.ResourceID {
			continue
		}
		s.resolvePending(ctx, acq, op)
	}
}

func (s *Scheduler) resolvePending(ctx context.Context, acq *storage.Acquisition, op *storage.Operation) {
	nowMs := s.clock.Now().UnixMilli()
	switch op.State {
	case storage.OpSucceeded:
		want, exclusive, err := wantOf(acq)
		if err != nil {
			return
		}
		// The bind re-runs the full reservation tx — capacity is
		// re-checked, never assumed (05 §5).
		if _, err := s.reserve(ctx, acq, *acq.PendingResourceID, want, exclusive); err != nil {
			s.log.Error("pre-bound reservation failed", "acquisition_id", acq.ID, "error", err)
		}
	case storage.OpFailed, storage.OpAborted:
		if op.Kind == storage.OpKindStart {
			// A failed START is not a failed acquisition: the machine
			// reverted to parked; the acquisition falls back through the
			// ladder on the next sweep (backoff keeps it off this machine).
			s.rePend(ctx, acq, nowMs)
			return
		}
		_ = s.st.Tx(ctx, func(tx storage.TxStore) error {
			return tx.Acquisitions().Transition(ctx, acq.ID, storage.AcqProvisioning, storage.AcqFailed, nowMs)
		})
	}
}

// rePend returns a provisioning acquisition to pending with no pre-bind.
func (s *Scheduler) rePend(ctx context.Context, acq *storage.Acquisition, nowMs int64) {
	_ = s.st.Tx(ctx, func(tx storage.TxStore) error {
		if err := tx.Acquisitions().Transition(ctx, acq.ID, storage.AcqProvisioning, storage.AcqPending, nowMs); err != nil {
			return err
		}
		return tx.Acquisitions().ClearPendingResource(ctx, acq.ID)
	})
}

// Resume re-drives open acquisitions after a restart (plan R6): runs at
// boot after Engine.Resume and again from the periodic sweep.
func (s *Scheduler) Resume(ctx context.Context, pendingTimeout int64) error {
	open, err := s.st.Acquisitions().ListByState(ctx, storage.AcqPending, storage.AcqProvisioning)
	if err != nil {
		return err
	}
	nowMs := s.clock.Now().UnixMilli()
	for _, acq := range open {
		switch acq.State {
		case storage.AcqPending:
			// Satisfy BEFORE the expiry check: a queued acquisition at its
			// deadline must get its force-scale, not an expiry (design
			// verification finding). ErrNoCapacity here is terminal — the
			// caller isn't waiting, so fail fast instead of a silent extra
			// (pendingTimeout - maxWait) wait.
			if _, err := s.Satisfy(ctx, acq.ID); err != nil {
				if errors.Is(err, ErrNoCapacity) {
					_ = s.st.Tx(ctx, func(tx storage.TxStore) error {
						if err := tx.Acquisitions().Transition(ctx, acq.ID, storage.AcqPending, storage.AcqFailed, nowMs); err != nil {
							return err
						}
						return tx.Events().Append(ctx, &storage.Event{
							TS: nowMs, Actor: acq.Actor, Type: "acquisition.failed", Outcome: "no_capacity",
							Details: json.RawMessage(fmt.Sprintf(`{"acquisitionId":%q,"reason":"queue deadline elapsed without capacity or a scalable class"}`, acq.ID)),
						})
					})
					continue
				}
				s.log.Error("acquisition resume", "acquisition_id", acq.ID, "error", err)
			}
			if cur, err := s.st.Acquisitions().Get(ctx, acq.ID); err == nil && cur.State == storage.AcqPending &&
				pendingTimeout > 0 && nowMs-cur.CreatedAt > pendingTimeout {
				_ = s.st.Tx(ctx, func(tx storage.TxStore) error {
					return tx.Acquisitions().Transition(ctx, acq.ID, storage.AcqPending, storage.AcqExpired, nowMs)
				})
			}
		case storage.AcqProvisioning:
			// The provisioning clock restarts at the scale transition
			// (UpdatedAt): a queued acquisition that consumed most of its
			// pendingTimeout budget still gives its create a full runway.
			if pendingTimeout > 0 && nowMs-acq.UpdatedAt > pendingTimeout {
				_ = s.st.Tx(ctx, func(tx storage.TxStore) error {
					if err := tx.Acquisitions().Transition(ctx, acq.ID, storage.AcqProvisioning, storage.AcqExpired, nowMs); err != nil {
						return err
					}
					details := `{"acquisitionId":"` + string(acq.ID) + `"`
					if acq.PendingResourceID != nil {
						details += `,"orphanedPrebindResourceId":"` + string(*acq.PendingResourceID) + `"`
					}
					details += `}`
					return tx.Events().Append(ctx, &storage.Event{
						TS: nowMs, Actor: acq.Actor, Type: "acquisition.expired", Outcome: "provisioning_timeout",
						ResourceID: acq.PendingResourceID, Details: json.RawMessage(details),
					})
				})
				continue
			}
			if acq.PendingResourceID == nil {
				continue
			}
			res, err := s.st.Resources().Get(ctx, *acq.PendingResourceID)
			if err != nil {
				continue
			}
			switch res.Phase {
			case phase.Ready:
				s.resolvePending(ctx, acq, &storage.Operation{Kind: storage.OpKindCreate, ResourceID: acq.PendingResourceID, State: storage.OpSucceeded})
			case phase.Failed:
				s.resolvePending(ctx, acq, &storage.Operation{Kind: storage.OpKindCreate, ResourceID: acq.PendingResourceID, State: storage.OpFailed})
			case phase.Parked:
				// Crash window (docs/12 design finding): the start failed
				// and reverted before the terminal callback ran — re-pend
				// so the ladder retries instead of expiring the caller.
				s.rePend(ctx, acq, nowMs)
			case phase.Starting:
				// start in flight — the op terminal callback resolves it.
			}
		}
	}
	return nil
}

// wantOf derives the requested capacity and exclusivity from the request
// (capacity.ParseRequest is the single parser — docs/11 §8).
func wantOf(acq *storage.Acquisition) (capacity.Vector, bool, error) {
	_, want, exclusive, err := capacity.ParseRequest(acq.Constraints)
	return want, exclusive, err
}
