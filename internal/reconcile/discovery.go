package reconcile

// Discovery sweep (docs 02 §5, 07 §5; ADR-017): refresh observations, and
// resolve the three unmatched cases —
//
//   orphan   local record whose provider resource vanished: confirmed by a
//            direct Get, then tombstoned only after the grace window, with
//            ZERO active leases and a healthy provider (a cloud outage must
//            never reclaim a leased VM).
//   ghost    provider resource carrying OUR labels whose create operation
//            succeeded bound to a DIFFERENT server (duplicate-create race):
//            policy-gated journaled delete (default) or surface-only.
//   readopt  provider resource carrying OUR labels with no live record and
//            no journal evidence (restored/older database): re-adopted as
//            managed (06 §8 — never undeletable because local state was
//            lost); the class label restores its reclaim policy.
//
// Unlabeled resources are foreign: optionally recorded as observed
// (read-only; invariant 5), never mutated.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/samishal1998/fleetplane/internal/ids"
	"github.com/samishal1998/fleetplane/internal/phase"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
)

type DiscoveryConfig struct {
	Interval       time.Duration
	OrphanGrace    time.Duration // between orphan confirmation and tombstone
	GhostPolicy    string        // "delete" (default) | "surface"
	AdoptUnlabeled string        // "off" (default) | "observed"
}

func (c *DiscoveryConfig) defaults() {
	if c.Interval <= 0 {
		c.Interval = 30 * time.Second
	}
	if c.OrphanGrace <= 0 {
		c.OrphanGrace = 60 * time.Second
	}
	if c.GhostPolicy == "" {
		c.GhostPolicy = "delete"
	}
	if c.AdoptUnlabeled == "" {
		c.AdoptUnlabeled = "off"
	}
}

// Discovery wiring (set once at boot before Run).
type discovery struct {
	cfg     DiscoveryConfig
	names   func() []string
	healthy func(instance string) bool
}

// EnableDiscovery arms the sweep. healthy reports whether a provider
// instance is reliable enough for absence to mean anything.
func (r *Reconciler) EnableDiscovery(cfg DiscoveryConfig, names func() []string, healthy func(string) bool) {
	cfg.defaults()
	if healthy == nil {
		healthy = func(string) bool { return true }
	}
	r.disc = &discovery{cfg: cfg, names: names, healthy: healthy}
}

// RunDiscovery sweeps every provider instance until ctx is done.
func (r *Reconciler) RunDiscovery(ctx context.Context) {
	if r.disc == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.clock.After(r.disc.cfg.Interval):
		}
		for _, name := range r.disc.names() {
			if err := r.SweepProvider(ctx, name); err != nil && ctx.Err() == nil {
				r.log.Error("discovery sweep", "provider", name, "error", err)
			}
		}
	}
}

// SweepProvider runs one full discovery pass for one provider instance.
func (r *Reconciler) SweepProvider(ctx context.Context, instanceName string) error {
	if r.disc == nil {
		return fmt.Errorf("discovery not enabled")
	}
	r.sweepInstance = instanceName
	inst, ok := r.providers.Instance(storage.ProviderInstance(instanceName))
	if !ok {
		return fmt.Errorf("unknown provider instance %q", instanceName)
	}
	// Sweep every kind the provider declares (Phase 9: kind-agnostic).
	for _, kind := range inst.Descriptor().Kinds {
		driver, ok := inst.ResourceDriver(kind)
		if !ok {
			continue
		}
		if err := r.sweepKind(ctx, instanceName, kind, driver); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) sweepKind(ctx context.Context, instanceName string, kind provider.ResourceKind, driver provider.ResourceDriver) error {

	scope := provider.ScopeOwned
	if r.disc.cfg.AdoptUnlabeled == "observed" {
		scope = provider.ScopeAll
	}
	observed, err := driver.Discover(ctx, provider.DiscoverRequest{Scope: scope})
	if err != nil {
		return err
	}

	all, err := r.st.Resources().List(ctx, storage.ResourceFilter{
		Provider: storage.ProviderInstance(instanceName), Kind: string(kind), IncludeDeleted: true,
	})
	if err != nil {
		return err
	}
	liveByExt := map[string]*storage.Resource{}
	liveByID := map[storage.ResourceID]*storage.Resource{}
	for _, res := range all {
		if res.DeletedAt != nil {
			continue
		}
		liveByID[res.ID] = res
		if res.ExternalID != nil {
			liveByExt[*res.ExternalID] = res
		}
	}
	openOps, err := r.st.Operations().NonTerminal(ctx)
	if err != nil {
		return err
	}
	hasOpenOp := map[storage.ResourceID]bool{}
	for _, op := range openOps {
		if op.ResourceID != nil {
			hasOpenOp[*op.ResourceID] = true
		}
	}

	nowMs := r.clock.Now().UnixMilli()
	seen := map[string]bool{}
	for i := range observed {
		obs := &observed[i]
		seen[obs.Ref.ID] = true
		if res, ok := liveByExt[obs.Ref.ID]; ok {
			r.refreshObserved(ctx, res, obs, nowMs)
			continue
		}
		if obs.Owned {
			r.resolveOwnedUnmatched(ctx, obs, liveByID, hasOpenOp, nowMs)
			continue
		}
		if r.disc.cfg.AdoptUnlabeled == "observed" {
			r.adoptObserved(ctx, instanceName, obs, nowMs)
		}
	}

	// Local records absent from the listing: absence in a LIST is never
	// proof (05 §10) — confirm by direct Get, and only when healthy.
	if !r.disc.healthy(instanceName) {
		r.log.Warn("provider unhealthy; orphan confirmation suspended", "provider", instanceName)
		return nil
	}
	for _, res := range all {
		if res.DeletedAt != nil || res.ExternalID == nil || seen[*res.ExternalID] || hasOpenOp[res.ID] {
			continue
		}
		r.confirmOrphan(ctx, driver, res, nowMs)
	}
	return nil
}

func (r *Reconciler) refreshObserved(ctx context.Context, res *storage.Resource, obs *provider.ObservedResource, nowMs int64) {
	raw, _ := json.Marshal(obs)
	capJSON, _ := json.Marshal(obs.Capacity)
	_ = r.st.Tx(ctx, func(tx storage.TxStore) error {
		if err := tx.Resources().PutObserved(ctx, &storage.ObservedSnapshot{
			ResourceID: res.ID, ObservedAt: nowMs, ProviderPhase: string(obs.Phase), Raw: raw,
		}); err != nil {
			return err
		}
		if err := tx.Resources().SetProviderFacts(ctx, res.ID, obs.Extensions, capJSON, nowMs); err != nil {
			return err
		}
		// A re-observed orphan is alive again (05 §7). A formerly-parked
		// machine that reappears STOPPED revives to parked, with the
		// stage-2 clock restarted at observation time (docs/12).
		if res.Phase == phase.Orphaned && obs.Phase == provider.PhaseRunning {
			return tx.Resources().CASPhase(ctx, res.ID, phase.Orphaned, phase.Ready, nowMs)
		}
		if res.Phase == phase.Orphaned && obs.Phase == provider.PhaseStopped &&
			r.providers.Parking(res.Provider, res.Kind).Supported {
			return tx.Resources().CASPhase(ctx, res.ID, phase.Orphaned, phase.Parked, nowMs)
		}
		return nil
	})
}

// resolveOwnedUnmatched implements the ghost/re-adopt split (plan R8/R9).
func (r *Reconciler) resolveOwnedUnmatched(ctx context.Context, obs *provider.ObservedResource,
	liveByID map[storage.ResourceID]*storage.Resource, hasOpenOp map[storage.ResourceID]bool, nowMs int64) {

	// The verifier owns anything tied to an in-flight create (plan R10).
	if obs.FleetplaneID != "" {
		if res, ok := liveByID[storage.ResourceID(obs.FleetplaneID)]; ok && res.ExternalID == nil && hasOpenOp[res.ID] {
			return
		}
	}

	// Journal evidence decides ghost vs re-adopt.
	if obs.CreateOpID != "" {
		if op, err := r.st.Operations().Get(ctx, storage.OperationID(obs.CreateOpID)); err == nil {
			if !op.State.Terminal() {
				return // resolution in progress; hands off
			}
			boundElsewhere := false
			if op.State == storage.OpSucceeded && op.ResourceID != nil {
				if res, ok := liveByID[*op.ResourceID]; ok && res.ExternalID != nil && *res.ExternalID != obs.Ref.ID {
					boundElsewhere = true
				}
			}
			if boundElsewhere || op.State == storage.OpFailed || op.State == storage.OpAborted {
				r.handleGhost(ctx, obs, nowMs)
				return
			}
			// Succeeded op whose record was tombstoned/lost: restore
			// residue → re-adopt (R9).
		}
	}
	r.readopt(ctx, obs, nowMs)
}

func (r *Reconciler) handleGhost(ctx context.Context, obs *provider.ObservedResource, nowMs int64) {
	res := r.mintRecord(ctx, obs, storage.OwnershipManaged, nowMs, "ghost")
	if res == nil {
		return
	}
	if r.disc.cfg.GhostPolicy != "delete" {
		r.log.Warn("ghost surfaced (policy=surface); delete manually", "external_id", obs.Ref.ID)
		return
	}
	// Normal journaled, policy-gated delete — never a blind inline call.
	if err := r.casPhase(ctx, res.ID, phase.Ready, phase.Draining, nowMs); err == nil {
		if err := r.journalDelete(ctx, res, nowMs); err == nil {
			r.engine.Kick()
			r.log.Info("ghost reclaimed via journaled delete", "resource_id", res.ID, "external_id", obs.Ref.ID)
		}
	}
}

func (r *Reconciler) readopt(ctx context.Context, obs *provider.ObservedResource, nowMs int64) {
	res := r.mintRecord(ctx, obs, storage.OwnershipManaged, nowMs, "readopted")
	if res != nil {
		r.log.Info("re-adopted labeled provider resource as managed (06 §8)",
			"resource_id", res.ID, "external_id", obs.Ref.ID, "class", res.Class)
	}
}

func (r *Reconciler) adoptObserved(ctx context.Context, instanceName string, obs *provider.ObservedResource, nowMs int64) {
	_ = r.mintRecord(ctx, obs, storage.OwnershipObserved, nowMs, "observed")
}

func (r *Reconciler) mintRecord(ctx context.Context, obs *provider.ObservedResource, ownership storage.Ownership, nowMs int64, event string) *storage.Resource {
	extRef, _ := json.Marshal(obs.Ref)
	capJSON, _ := json.Marshal(obs.Capacity)
	extID := obs.Ref.ID
	spec := json.RawMessage(`{}`)
	res := &storage.Resource{
		ID: storage.ResourceID(ids.New(ids.Resource)), Name: obs.Labels["name"],
		Kind: string(obs.Kind), Provider: storage.ProviderInstance(r.instanceOf(obs)),
		Class:     obs.Labels[provider.LabelClass],
		Ownership: ownership, Phase: phase.Ready,
		ExternalID: &extID, ExternalRef: extRef,
		Spec: spec, Extension: obs.Extensions, Capacity: capJSON,
		Labels:    obs.Labels,
		CreatedAt: nowMs, UpdatedAt: nowMs,
	}
	// The initial phase follows the OBSERVATION (docs/12, design finding):
	// a restore/re-adopt of a stopped fleet must not mint schedulable
	// "ready" records for machines that are powered off.
	if obs.Phase == provider.PhaseStopped && r.providers.Parking(res.Provider, res.Kind).Supported {
		res.Phase = phase.Parked
		res.ParkedAt = &nowMs
	}
	err := r.st.Tx(ctx, func(tx storage.TxStore) error {
		if err := tx.Resources().Create(ctx, res); err != nil {
			return err
		}
		return tx.Events().Append(ctx, &storage.Event{
			TS: nowMs, Actor: "discovery", Type: "resource." + event,
			ResourceID: &res.ID, Provider: res.Provider, Outcome: event,
			Details: json.RawMessage(fmt.Sprintf(`{"externalId":%q}`, extID)),
		})
	})
	if err != nil {
		r.log.Error("minting discovered record", "external_id", extID, "error", err)
		return nil
	}
	return res
}

// instanceOf: the sweep runs per instance; observations don't carry it, so
// stash it on the reconciler during the sweep. (Single-threaded per sweep.)
func (r *Reconciler) instanceOf(*provider.ObservedResource) string { return r.sweepInstance }

// confirmOrphan applies the ADR-017 gates: direct-Get confirmation, grace
// window, zero active leases.
func (r *Reconciler) confirmOrphan(ctx context.Context, driver provider.ResourceDriver, res *storage.Resource, nowMs int64) {
	var ref provider.ExternalRef
	if len(res.ExternalRef) > 0 {
		_ = json.Unmarshal(res.ExternalRef, &ref)
	}
	if ref.ID == "" {
		ref.ID = *res.ExternalID
	}
	_, err := driver.Get(ctx, ref)
	if err == nil || !provider.IsClass(err, provider.ErrNotFound) {
		return // present, or indeterminate — never act on uncertainty
	}
	switch res.Phase {
	case phase.Orphaned:
		if nowMs-res.UpdatedAt < r.disc.cfg.OrphanGrace.Milliseconds() {
			return // inside the grace window
		}
		_ = r.st.Tx(ctx, func(tx storage.TxStore) error {
			n, err := tx.Leases().CountActive(ctx, res.ID)
			if err != nil {
				return err
			}
			if n > 0 {
				// Invariant 3's spirit: a leased record is never
				// reclaimed, even when the provider lost the machine.
				return fmt.Errorf("%w: %d active leases", storage.ErrConflict, n)
			}
			if err := tx.Resources().MarkDeleted(ctx, res.ID, nowMs); err != nil {
				return err
			}
			return tx.Events().Append(ctx, &storage.Event{
				TS: nowMs, Actor: "discovery", Type: "resource.orphan_tombstoned",
				ResourceID: &res.ID, Provider: res.Provider, Outcome: "tombstoned",
			})
		})
	default:
		if !phase.CanTransition(res.Phase, phase.Orphaned) {
			return
		}
		if err := r.casPhase(ctx, res.ID, res.Phase, phase.Orphaned, nowMs); err == nil {
			_ = r.st.Tx(ctx, func(tx storage.TxStore) error {
				return tx.Events().Append(ctx, &storage.Event{
					TS: nowMs, Actor: "discovery", Type: "resource.orphaned",
					ResourceID: &res.ID, Provider: res.Provider, Outcome: "orphaned",
				})
			})
			r.log.Warn("resource orphaned (provider lost it); tombstone after grace",
				"resource_id", res.ID, "external_id", *res.ExternalID)
		}
	}
}
