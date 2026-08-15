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

	"github.com/samishal1998/fleetplane/internal/capacity"
	"github.com/samishal1998/fleetplane/internal/ids"
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
}

func New(st storage.Store, providers provision.Providers, engine Kicker,
	classes reconcile.ClassResolver, clock sdk.Clock, log *slog.Logger, ownerID string) *Scheduler {
	return &Scheduler{st: st, providers: providers, engine: engine, classes: classes,
		clock: clock, log: log, ownerID: ownerID}
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
	want, exclusive, err := wantOf(acq)
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
			return s.st.Acquisitions().Get(ctx, acqID)
		}
		if !errors.Is(err, storage.ErrConflict) && !errors.Is(err, errInsufficient) {
			return nil, err
		}
	}
	// Step 8: scale on demand (needs a class).
	return s.scaleOnDemand(ctx, acq)
}

type candidate struct {
	ID    storage.ResourceID
	score int64
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
		out = append(out, candidate{ID: res.ID, score: capacity.FreeAfter(total, used, want)})
	}
	// Deterministic best-fit: tightest fit first, ULID tie-break.
	sort.Slice(out, func(i, j int) bool {
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
// lands (wired into engine.OnTerminal alongside the reconciler).
func (s *Scheduler) HandleOpTerminal(op *storage.Operation) {
	if op.Kind != storage.OpKindCreate || op.ResourceID == nil {
		return
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
		s.resolvePending(ctx, acq, op.State)
	}
}

func (s *Scheduler) resolvePending(ctx context.Context, acq *storage.Acquisition, opState storage.OpState) {
	nowMs := s.clock.Now().UnixMilli()
	switch opState {
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
		_ = s.st.Tx(ctx, func(tx storage.TxStore) error {
			return tx.Acquisitions().Transition(ctx, acq.ID, storage.AcqProvisioning, storage.AcqFailed, nowMs)
		})
	}
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
		// Expire never-satisfied acquisitions past the timeout.
		if pendingTimeout > 0 && nowMs-acq.CreatedAt > pendingTimeout {
			_ = s.st.Tx(ctx, func(tx storage.TxStore) error {
				return tx.Acquisitions().Transition(ctx, acq.ID, acq.State, storage.AcqExpired, nowMs)
			})
			continue
		}
		switch acq.State {
		case storage.AcqPending:
			if _, err := s.Satisfy(ctx, acq.ID); err != nil && !errors.Is(err, ErrNoCapacity) {
				s.log.Error("acquisition resume", "acquisition_id", acq.ID, "error", err)
			}
		case storage.AcqProvisioning:
			if acq.PendingResourceID == nil {
				continue
			}
			res, err := s.st.Resources().Get(ctx, *acq.PendingResourceID)
			if err != nil {
				continue
			}
			switch res.Phase {
			case phase.Ready:
				s.resolvePending(ctx, acq, storage.OpSucceeded)
			case phase.Failed:
				s.resolvePending(ctx, acq, storage.OpFailed)
			}
		}
	}
	return nil
}

// wantOf derives the requested capacity and exclusivity from the request.
func wantOf(acq *storage.Acquisition) (capacity.Vector, bool, error) {
	var req struct {
		Exclusive   bool            `json:"exclusive"`
		Constraints json.RawMessage `json:"constraints"`
	}
	if len(acq.Constraints) > 0 {
		if err := json.Unmarshal(acq.Constraints, &req); err != nil {
			return nil, false, fmt.Errorf("acquisition constraints: %w", err)
		}
	}
	want, err := capacity.WantFromConstraints(req.Constraints)
	if err != nil {
		return nil, false, err
	}
	// No dimensioned request at all = whole-machine semantics.
	exclusive := req.Exclusive || len(want) == 0
	return want, exclusive, nil
}
