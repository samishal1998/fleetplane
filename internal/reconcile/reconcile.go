// Package reconcile is the pool reconciler (docs/05): level-triggered,
// per-pool serialized convergence of observed fleet size toward the pool
// spec, with bounded mutations per cycle, cooldown/backoff, drain/undrain
// and pool idle reclaim.
package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/samishal1998/fleetplane/internal/ids"
	"github.com/samishal1998/fleetplane/internal/phase"
	"github.com/samishal1998/fleetplane/internal/provision"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/pkg/kinds/compute"
	"github.com/samishal1998/fleetplane/pkg/sdk"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
)

// PoolSpec is the pools.spec_json shape (04 §5).
type PoolSpec struct {
	Class        string           `json:"class"`
	Replicas     int              `json:"replicas"`
	MinReady     int              `json:"minReady,omitempty"`
	MaxResources int              `json:"maxResources,omitempty"`
	Reclaim      *ReclaimPolicy   `json:"reclaim,omitempty"`
	Machine      *json.RawMessage `json:"machine,omitempty"` // inline spec when no class
	Provider     string           `json:"provider,omitempty"`
	Kind         string           `json:"kind,omitempty"`
}

type ReclaimPolicy struct {
	IdleAfter compute.Duration `json:"idleAfter,omitempty"`
}

// Class is a resolved class template (04 §2).
type Class struct {
	Kind     string
	Provider string
	Spec     json.RawMessage
	Reclaim  *ReclaimPolicy // optional class-level idle policy (plan R21)
}

// ClassResolver resolves class names (config-owned).
type ClassResolver interface {
	Class(name string) (Class, bool)
}

// Kicker wakes the operation engine after journaling.
type Kicker interface{ Kick() }

type Config struct {
	Interval             time.Duration // periodic level trigger
	MaxMutationsPerCycle int           // R22: one budget knob
	FailureBackoffBase   time.Duration
	FailureBackoffCap    time.Duration
}

func (c *Config) defaults() {
	if c.Interval <= 0 {
		c.Interval = 15 * time.Second
	}
	if c.MaxMutationsPerCycle <= 0 {
		c.MaxMutationsPerCycle = 5
	}
	if c.FailureBackoffBase <= 0 {
		c.FailureBackoffBase = 5 * time.Second
	}
	if c.FailureBackoffCap <= 0 {
		c.FailureBackoffCap = 10 * time.Minute
	}
}

type Reconciler struct {
	st        storage.Store
	providers provision.Providers
	engine    Kicker
	classes   ClassResolver
	clock     sdk.Clock
	log       *slog.Logger
	cfg       Config
	ownerID   string

	mu    sync.Mutex
	dirty map[storage.PoolID]bool
	wake  chan struct{}

	disc          *discovery
	sweepInstance string // instance under sweep (sweeps are serialized)
}

func New(st storage.Store, providers provision.Providers, engine Kicker, classes ClassResolver,
	clock sdk.Clock, log *slog.Logger, ownerID string, cfg Config) *Reconciler {
	cfg.defaults()
	return &Reconciler{
		st: st, providers: providers, engine: engine, classes: classes,
		clock: clock, log: log, ownerID: ownerID, cfg: cfg,
		dirty: map[storage.PoolID]bool{}, wake: make(chan struct{}, 1),
	}
}

// Kick marks one pool dirty (level-triggered, coalescing). A nil dirty map
// means "everything is already dirty" (KickAll) — nothing to add.
func (r *Reconciler) Kick(pool storage.PoolID) {
	r.mu.Lock()
	if r.dirty != nil {
		r.dirty[pool] = true
	}
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// KickAll marks every pool dirty.
func (r *Reconciler) KickAll() {
	r.mu.Lock()
	r.dirty = nil // nil means "all"
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// HandleOpTerminal is wired as the engine's OnTerminal callback: pool ops
// re-trigger their pool; create failures bump the pool's failure backoff.
func (r *Reconciler) HandleOpTerminal(op *storage.Operation) {
	if op.PoolID == nil {
		return
	}
	if op.Kind == storage.OpKindCreate && op.State == storage.OpFailed {
		r.bumpFailureBackoff(context.Background(), *op.PoolID)
	}
	if op.Kind == storage.OpKindCreate && op.State == storage.OpSucceeded {
		r.resetFailureBackoff(context.Background(), *op.PoolID)
	}
	r.Kick(*op.PoolID)
}

// Run reconciles until ctx is done.
func (r *Reconciler) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-r.clock.After(r.cfg.Interval):
			r.mu.Lock()
			r.dirty = nil // periodic pass touches everything
			r.mu.Unlock()
		}
		r.mu.Lock()
		todo := r.dirty
		r.dirty = map[storage.PoolID]bool{}
		r.mu.Unlock()

		pools, err := r.st.Pools().List(ctx)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Error("reconcile: list pools", "error", err)
			}
			continue
		}
		for _, p := range pools {
			if todo != nil && !todo[p.ID] {
				continue
			}
			if _, err := r.RunOnce(ctx, p.ID); err != nil && ctx.Err() == nil {
				r.log.Error("reconcile pool", "pool_id", p.ID, "error", err)
			}
		}
		if n, err := r.ReclaimPoolless(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("poolless reclaim", "error", err)
		} else if n > 0 {
			r.engine.Kick()
		}
	}
}

// ReclaimPoolless reclaims idle poolless resources whose CLASS declares an
// idle policy (plan R21) — this is what makes the 08 §7 demo's "idle policy
// eventually deletes" work without a pool. Returns deletes journaled.
func (r *Reconciler) ReclaimPoolless(ctx context.Context) (int, error) {
	list, err := r.st.Resources().List(ctx, storage.ResourceFilter{
		Poolless:  true,
		Phases:    []phase.Phase{phase.Ready, phase.Draining},
		Ownership: []storage.Ownership{storage.OwnershipManaged},
	})
	if err != nil {
		return 0, err
	}
	nowMs := r.clock.Now().UnixMilli()
	budget := r.cfg.MaxMutationsPerCycle
	n := 0
	for _, res := range list {
		if budget <= 0 {
			break
		}
		if res.Phase == phase.Draining {
			if err := r.journalDelete(ctx, res, nowMs); err == nil {
				n++
				budget--
			}
			continue
		}
		cls, ok := r.classes.Class(res.Class)
		if !ok || cls.Reclaim == nil || cls.Reclaim.IdleAfter.Std() <= 0 {
			continue // no policy = never auto-reclaimed
		}
		if res.DeleteProtected {
			continue
		}
		if nowMs-idleSinceOf(res) < cls.Reclaim.IdleAfter.Std().Milliseconds() {
			continue
		}
		if cnt, err := r.st.Leases().CountActive(ctx, res.ID); err != nil || cnt > 0 {
			continue // invariant 3
		}
		if err := r.casPhase(ctx, res.ID, phase.Ready, phase.Draining, nowMs); err == nil {
			if err := r.journalDelete(ctx, res, nowMs); err == nil {
				n++
				budget--
			}
		}
	}
	return n, nil
}

// Delta reports what one cycle decided.
type Delta struct {
	Counted, Ready, Provisioning, Allocated, Draining, Failed int
	Created, Drained, Undrained, Deleted                      int
}

type checkpoint struct {
	FailureAttempt     int   `json:"failureAttempt"`
	FailureBackoffTill int64 `json:"failureBackoffUntil"`
}

// RunOnce reconciles one pool one step (bounded mutations; convergence is
// the composition of repeated level-triggered cycles).
func (r *Reconciler) RunOnce(ctx context.Context, poolID storage.PoolID) (Delta, error) {
	var d Delta
	pool, err := r.st.Pools().Get(ctx, poolID)
	if err != nil {
		return d, err
	}
	if pool.Paused {
		return d, nil
	}
	var spec PoolSpec
	if err := json.Unmarshal(pool.Spec, &spec); err != nil {
		return d, fmt.Errorf("pool %s spec: %w", poolID, err)
	}
	kind, providerName, machineSpec, reclaim, err := r.template(spec)
	if err != nil {
		return d, err
	}

	all, err := r.st.Resources().List(ctx, storage.ResourceFilter{PoolID: &poolID})
	if err != nil {
		return d, err
	}
	byPhase := map[phase.Phase][]*storage.Resource{}
	for _, res := range all {
		byPhase[res.Phase] = append(byPhase[res.Phase], res)
	}
	d.Ready = len(byPhase[phase.Ready])
	d.Provisioning = len(byPhase[phase.Provisioning])
	d.Allocated = len(byPhase[phase.Allocated])
	d.Draining = len(byPhase[phase.Draining])
	d.Failed = len(byPhase[phase.Failed])
	// provisioning rows ARE the pending creates (co-created with their
	// journaled op — invariant 4, plan R20).
	d.Counted = d.Provisioning + d.Ready + d.Allocated

	budget := r.cfg.MaxMutationsPerCycle
	nowMs := r.clock.Now().UnixMilli()

	// --- undrain: deficit reappeared while draining (05 §6) ---
	deficit := spec.Replicas - d.Counted
	for _, res := range byPhase[phase.Draining] {
		if deficit <= 0 || budget <= 0 {
			break
		}
		if err := r.casPhase(ctx, res.ID, phase.Draining, phase.Ready, nowMs); err == nil {
			d.Undrained++
			d.Counted++
			deficit--
			budget--
		}
	}

	// --- scale up ---
	if deficit > 0 && budget > 0 {
		if until := r.failureBackoffUntil(ctx, poolID); nowMs < until {
			r.log.Info("pool creates paused by failure backoff", "pool_id", poolID)
		} else {
			liveTotal := len(all) // every non-tombstoned row counts against maxResources
			n := deficit
			if spec.MaxResources > 0 && liveTotal+n > spec.MaxResources {
				n = spec.MaxResources - liveTotal
			}
			n = min(n, budget)
			for i := 0; i < n; i++ {
				if err := r.createOne(ctx, pool, kind, providerName, machineSpec, nowMs); err != nil {
					r.log.Error("pool create", "pool_id", poolID, "error", err)
					break
				}
				d.Created++
				budget--
			}
			if d.Created > 0 {
				r.engine.Kick()
			}
		}
	}

	// --- scale down: drain surplus, longest-idle first, keep minReady ---
	surplus := d.Counted - spec.Replicas
	if surplus > 0 && budget > 0 {
		candidates := reclaimable(byPhase[phase.Ready], reclaim, nowMs)
		for _, res := range candidates {
			if surplus <= 0 || budget <= 0 {
				break
			}
			if d.Ready-d.Drained-1 < spec.MinReady {
				break
			}
			if n, err := r.st.Leases().CountActive(ctx, res.ID); err != nil || n > 0 {
				continue // invariant 3: leased resources are never reclaimed
			}
			if err := r.casPhase(ctx, res.ID, phase.Ready, phase.Draining, nowMs); err == nil {
				d.Drained++
				surplus--
				budget--
			}
		}
	}

	// --- drain pipeline: leases==0 → journal delete (05 §6) ---
	for _, res := range byPhase[phase.Draining] {
		if budget <= 0 {
			break
		}
		if err := r.journalDelete(ctx, res, nowMs); err == nil {
			d.Deleted++
			budget--
		}
	}
	// --- failed cleanup: journal delete so deficit math replaces them ---
	for _, res := range byPhase[phase.Failed] {
		if budget <= 0 {
			break
		}
		if err := r.journalDelete(ctx, res, nowMs); err == nil {
			d.Deleted++
			budget--
		}
	}
	if d.Deleted > 0 {
		r.engine.Kick()
	}
	return d, nil
}

func (r *Reconciler) template(spec PoolSpec) (kind, providerName string, machine json.RawMessage, reclaim *ReclaimPolicy, err error) {
	reclaim = spec.Reclaim
	if spec.Class != "" {
		cls, ok := r.classes.Class(spec.Class)
		if !ok {
			return "", "", nil, nil, fmt.Errorf("unknown class %q", spec.Class)
		}
		if reclaim == nil {
			reclaim = cls.Reclaim
		}
		return cls.Kind, cls.Provider, cls.Spec, reclaim, nil
	}
	if spec.Machine == nil || spec.Provider == "" || spec.Kind == "" {
		return "", "", nil, nil, fmt.Errorf("pool spec needs either class or kind+provider+machine")
	}
	return spec.Kind, spec.Provider, *spec.Machine, reclaim, nil
}

func (r *Reconciler) createOne(ctx context.Context, pool *storage.Pool, kind, providerName string, machine json.RawMessage, nowMs int64) error {
	prepared, err := provision.Prepare(ctx, r.providers, r.ownerID, provision.CreateSpec{
		Kind: kind, Provider: providerName,
		Spec: machine, Class: classNameOf(pool), PoolID: &pool.ID,
		Labels: map[string]string{"fleetplane.io/pool": pool.Name},
	}, nowMs)
	if err != nil {
		return err
	}
	return r.st.Tx(ctx, func(tx storage.TxStore) error {
		return prepared.Journal(ctx, tx, "reconciler", "")
	})
}

// journalDelete journals a provider delete with every deletion gate
// re-checked INSIDE the transaction (invariants 3, 5).
func (r *Reconciler) journalDelete(ctx context.Context, res *storage.Resource, nowMs int64) error {
	return r.st.Tx(ctx, func(tx storage.TxStore) error {
		cur, err := tx.Resources().Get(ctx, res.ID)
		if err != nil {
			return err
		}
		if cur.DeletedAt != nil || (cur.Phase != phase.Draining && cur.Phase != phase.Failed) {
			return fmt.Errorf("%w: not deletable", storage.ErrConflict)
		}
		if cur.Ownership != storage.OwnershipManaged || cur.DeleteProtected {
			return fmt.Errorf("%w: ownership/protection gate", storage.ErrConflict)
		}
		if n, err := tx.Leases().CountActive(ctx, res.ID); err != nil {
			return err
		} else if n > 0 {
			return fmt.Errorf("%w: active leases", storage.ErrConflict)
		}
		if cur.ExternalID == nil {
			// Never materialized: tombstone directly (any ghost carries
			// our op label and is collapsed by discovery, plan R8).
			return tx.Resources().MarkDeleted(ctx, res.ID, nowMs)
		}
		if err := tx.Resources().CASPhase(ctx, res.ID, cur.Phase, phase.Deleting, nowMs); err != nil {
			return err
		}
		var ref provider.ExternalRef
		if len(cur.ExternalRef) > 0 {
			_ = json.Unmarshal(cur.ExternalRef, &ref)
		}
		if ref.ID == "" {
			ref.ID = *cur.ExternalID
		}
		opID := newOpID()
		action := provider.Action{ActionID: string(opID), Kind: "delete", ResourceID: string(res.ID), Ref: &ref, Destructive: true}
		actionJSON, _ := json.Marshal(action)
		op := &storage.Operation{
			ID: opID, Kind: storage.OpKindDelete, ResourceID: &res.ID, PoolID: cur.PoolID,
			Provider: cur.Provider, Action: actionJSON, State: storage.OpJournaled,
			CreatedAt: nowMs, UpdatedAt: nowMs,
		}
		if err := tx.Operations().Append(ctx, op); err != nil {
			return err
		}
		return tx.Events().Append(ctx, &storage.Event{
			TS: nowMs, Actor: "reconciler", Type: "resource.delete",
			ResourceID: &res.ID, PoolID: cur.PoolID, OperationID: &opID,
			Provider: cur.Provider, Outcome: "journaled",
		})
	})
}

func (r *Reconciler) casPhase(ctx context.Context, id storage.ResourceID, from, to phase.Phase, nowMs int64) error {
	return r.st.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Resources().CASPhase(ctx, id, from, to, nowMs)
	})
}

// reclaimable returns ready resources whose idle time exceeds the policy,
// longest-idle first (already sorted by construction: List orders by id =
// creation order; we sort by idle explicitly).
func reclaimable(ready []*storage.Resource, policy *ReclaimPolicy, nowMs int64) []*storage.Resource {
	idleAfter := int64(0)
	if policy != nil {
		idleAfter = policy.IdleAfter.Std().Milliseconds()
	}
	var out []*storage.Resource
	for _, res := range ready {
		if res.DeleteProtected {
			continue
		}
		idleSince := res.CreatedAt
		if res.ReadyAt != nil {
			idleSince = *res.ReadyAt
		}
		if res.LastLeaseEndedAt != nil && *res.LastLeaseEndedAt > idleSince {
			idleSince = *res.LastLeaseEndedAt
		}
		if nowMs-idleSince >= idleAfter {
			out = append(out, res)
		}
	}
	// longest idle first
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && idleSinceOf(out[j]) < idleSinceOf(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func idleSinceOf(r *storage.Resource) int64 {
	s := r.CreatedAt
	if r.ReadyAt != nil {
		s = *r.ReadyAt
	}
	if r.LastLeaseEndedAt != nil && *r.LastLeaseEndedAt > s {
		s = *r.LastLeaseEndedAt
	}
	return s
}

// --- failure backoff (persisted, per pool) ---

func (r *Reconciler) failureBackoffUntil(ctx context.Context, pool storage.PoolID) int64 {
	raw, err := r.st.Checkpoints().Get(ctx, "pool:"+string(pool))
	if err != nil {
		return 0
	}
	var cp checkpoint
	_ = json.Unmarshal(raw, &cp)
	return cp.FailureBackoffTill
}

func (r *Reconciler) bumpFailureBackoff(ctx context.Context, pool storage.PoolID) {
	key := "pool:" + string(pool)
	_ = r.st.Tx(ctx, func(tx storage.TxStore) error {
		var cp checkpoint
		if raw, err := tx.Checkpoints().Get(ctx, key); err == nil {
			_ = json.Unmarshal(raw, &cp)
		}
		cp.FailureAttempt++
		delay := r.cfg.FailureBackoffBase << min(cp.FailureAttempt, 12)
		if delay > r.cfg.FailureBackoffCap || delay <= 0 {
			delay = r.cfg.FailureBackoffCap
		}
		cp.FailureBackoffTill = r.clock.Now().UnixMilli() + delay.Milliseconds()
		raw, _ := json.Marshal(cp)
		return tx.Checkpoints().Put(ctx, key, raw, r.clock.Now().UnixMilli())
	})
}

func (r *Reconciler) resetFailureBackoff(ctx context.Context, pool storage.PoolID) {
	key := "pool:" + string(pool)
	_ = r.st.Tx(ctx, func(tx storage.TxStore) error {
		raw, _ := json.Marshal(checkpoint{})
		return tx.Checkpoints().Put(ctx, key, raw, r.clock.Now().UnixMilli())
	})
}

func newOpID() storage.OperationID { return storage.OperationID(ids.New(ids.Operation)) }

func classNameOf(pool *storage.Pool) string {
	var spec PoolSpec
	_ = json.Unmarshal(pool.Spec, &spec)
	return spec.Class
}
