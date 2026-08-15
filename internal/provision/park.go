package provision

// Stop/start journaling (docs/12): the single-transaction cores shared by
// the reclaim sweep, the pool warm tier, the scheduler's start-before-create
// rung, and the explicit :park/:start API. Every gate is re-checked inside
// the caller's open transaction (the doc-11 blocker pattern) — on any gate
// failure the resource keeps its current phase and stays schedulable.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/samishal1998/fleetplane/internal/ids"
	"github.com/samishal1998/fleetplane/internal/phase"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
)

// ErrParkUnsupported: the provider has no stop/resume capability for the
// kind — a provider that cannot park must never see a stop action
// (docs/12 invariant 6).
var ErrParkUnsupported = errors.New("provider does not support parking this kind")

// JournalStop CASes ready→parking and journals the stop operation inside
// tx. requireUnprotected: the reclaim sweep skips delete-protected machines
// (hands-off flag); the explicit operator :park may bypass deliberately.
func JournalStop(ctx context.Context, tx storage.TxStore, providers Providers, resID storage.ResourceID, nowMs int64, actor string, requireUnprotected bool) (*storage.Operation, error) {
	cur, err := tx.Resources().Get(ctx, resID)
	if err != nil {
		return nil, err
	}
	if cur.DeletedAt != nil || cur.Phase != phase.Ready || cur.Ownership != storage.OwnershipManaged {
		return nil, fmt.Errorf("%w: resource %s is not a parkable ready managed machine", storage.ErrConflict, resID)
	}
	if requireUnprotected && cur.DeleteProtected {
		return nil, fmt.Errorf("%w: delete-protected", storage.ErrConflict)
	}
	if !providers.Parking(cur.Provider, cur.Kind).Supported {
		return nil, fmt.Errorf("%w (%s/%s)", ErrParkUnsupported, cur.Provider, cur.Kind)
	}
	if n, err := tx.Leases().CountActive(ctx, resID); err != nil {
		return nil, err
	} else if n > 0 {
		return nil, fmt.Errorf("%w: active leases", storage.ErrConflict) // invariant 3
	}
	return journalPhaseOp(ctx, tx, cur, nowMs, actor, phase.Ready, phase.Parking, storage.OpKindStop, "stop")
}

// JournalStart CASes parked→starting and journals the start operation
// inside tx (the CAS serializes concurrent claimers — losers fall through
// the scheduler ladder).
func JournalStart(ctx context.Context, tx storage.TxStore, providers Providers, resID storage.ResourceID, nowMs int64, actor string) (*storage.Operation, error) {
	cur, err := tx.Resources().Get(ctx, resID)
	if err != nil {
		return nil, err
	}
	if cur.DeletedAt != nil || cur.Phase != phase.Parked || cur.Ownership != storage.OwnershipManaged {
		return nil, fmt.Errorf("%w: resource %s is not a parked managed machine", storage.ErrConflict, resID)
	}
	if !providers.Parking(cur.Provider, cur.Kind).Supported {
		return nil, fmt.Errorf("%w (%s/%s)", ErrParkUnsupported, cur.Provider, cur.Kind)
	}
	return journalPhaseOp(ctx, tx, cur, nowMs, actor, phase.Parked, phase.Starting, storage.OpKindStart, "start")
}

func journalPhaseOp(ctx context.Context, tx storage.TxStore, cur *storage.Resource, nowMs int64, actor string, from, to phase.Phase, opKind storage.OpKind, actionKind string) (*storage.Operation, error) {
	if cur.ExternalID == nil {
		return nil, fmt.Errorf("%w: resource has no external identity", storage.ErrConflict)
	}
	var ref provider.ExternalRef
	if len(cur.ExternalRef) > 0 {
		_ = json.Unmarshal(cur.ExternalRef, &ref)
	}
	if ref.ID == "" {
		ref.ID = *cur.ExternalID
	}
	if err := tx.Resources().CASPhase(ctx, cur.ID, from, to, nowMs); err != nil {
		return nil, err
	}
	opID := storage.OperationID(ids.New(ids.Operation))
	action := provider.Action{ActionID: string(opID), Kind: actionKind, ResourceID: string(cur.ID), Ref: &ref}
	actionJSON, _ := json.Marshal(action)
	op := &storage.Operation{
		ID: opID, Kind: opKind, ResourceID: &cur.ID, PoolID: cur.PoolID,
		Provider: cur.Provider, Action: actionJSON, State: storage.OpJournaled,
		CreatedAt: nowMs, UpdatedAt: nowMs,
	}
	if err := tx.Operations().Append(ctx, op); err != nil {
		return nil, err
	}
	if err := tx.Events().Append(ctx, &storage.Event{
		TS: nowMs, Actor: actor, Type: "resource." + actionKind,
		ResourceID: &cur.ID, PoolID: cur.PoolID, OperationID: &opID,
		Provider: cur.Provider, Outcome: "journaled",
	}); err != nil {
		return nil, err
	}
	return op, nil
}
