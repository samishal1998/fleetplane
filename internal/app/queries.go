package app

import (
	"context"
	"fmt"

	"github.com/samishal1998/fleetplane/internal/phase"
	"github.com/samishal1998/fleetplane/internal/storage"
)

func (s *Service) GetOperation(ctx context.Context, id string) (*storage.Operation, error) {
	return s.st.Operations().Get(ctx, storage.OperationID(id))
}

// ListOperations returns open operations (terminal ones are visible via
// GetOperation and events; a full history listing is an ADR-API follow-up).
func (s *Service) ListOperations(ctx context.Context) ([]*storage.Operation, error) {
	return s.st.Operations().NonTerminal(ctx)
}

func (s *Service) ListEvents(ctx context.Context, f storage.EventFilter, limit int) ([]*storage.Event, error) {
	return s.st.Events().List(ctx, f, limit)
}

// DrainResource marks a resource draining (04 §3 :drain): it stops being a
// scheduling candidate; deletion happens via the drain pipeline once its
// leases end (05 §6).
func (s *Service) DrainResource(ctx context.Context, id, actor string) error {
	nowMs := s.clock.Now().UnixMilli()
	return s.st.Tx(ctx, func(tx storage.TxStore) error {
		res, err := tx.Resources().Get(ctx, storage.ResourceID(id))
		if err != nil {
			return err
		}
		if res.DeletedAt != nil {
			return fmt.Errorf("%w: resource already deleted", storage.ErrNotFound)
		}
		if !phase.CanTransition(res.Phase, phase.Draining) {
			return fmt.Errorf("%w: resource in phase %s cannot drain", storage.ErrConflict, res.Phase)
		}
		if err := tx.Resources().CASPhase(ctx, res.ID, res.Phase, phase.Draining, nowMs); err != nil {
			return err
		}
		return tx.Events().Append(ctx, &storage.Event{
			TS: nowMs, Actor: actor, Type: "resource.drain", ResourceID: &res.ID,
			Provider: res.Provider, Outcome: "draining",
		})
	})
}

// UndrainResource returns a draining resource to service — the manual inverse
// of :drain, for the drain that was a mistake. It mirrors the reconciler's
// automatic undrain (05 §6) by going through the same CASPhase, so ready_at is
// restamped and the idle sweeper measures from the moment the resource came
// back rather than from before the drain. Racing the reconciler is safe: one
// CAS wins and the other reports a conflict.
func (s *Service) UndrainResource(ctx context.Context, id, actor string) error {
	nowMs := s.clock.Now().UnixMilli()
	return s.st.Tx(ctx, func(tx storage.TxStore) error {
		res, err := tx.Resources().Get(ctx, storage.ResourceID(id))
		if err != nil {
			return err
		}
		if res.DeletedAt != nil {
			return fmt.Errorf("%w: resource already deleted", storage.ErrNotFound)
		}
		if res.Phase == phase.Ready {
			return ErrAlreadyThere
		}
		if res.Phase != phase.Draining {
			return fmt.Errorf("%w: resource in phase %s is not draining", storage.ErrConflict, res.Phase)
		}
		if err := tx.Resources().CASPhase(ctx, res.ID, phase.Draining, phase.Ready, nowMs); err != nil {
			return err
		}
		return tx.Events().Append(ctx, &storage.Event{
			TS: nowMs, Actor: actor, Type: "resource.undrain", ResourceID: &res.ID,
			Provider: res.Provider, Outcome: "ready",
		})
	})
}

// SetResourceProtected toggles the deletion guard. A protected resource
// refuses deletion, parking, exclusive scheduling and idle reclaim until it is
// explicitly unprotected — the flag every one of those gates already reads,
// which until now nothing could set.
func (s *Service) SetResourceProtected(ctx context.Context, id string, protected bool, actor string) error {
	nowMs := s.clock.Now().UnixMilli()
	return s.st.Tx(ctx, func(tx storage.TxStore) error {
		res, err := tx.Resources().Get(ctx, storage.ResourceID(id))
		if err != nil {
			return err
		}
		if res.DeletedAt != nil {
			return fmt.Errorf("%w: resource already deleted", storage.ErrNotFound)
		}
		if res.DeleteProtected == protected {
			return ErrAlreadyThere
		}
		if err := tx.Resources().SetDeleteProtected(ctx, res.ID, protected, nowMs); err != nil {
			return err
		}
		evType := "resource.unprotected"
		if protected {
			evType = "resource.protected"
		}
		return tx.Events().Append(ctx, &storage.Event{
			TS: nowMs, Actor: actor, Type: evType, ResourceID: &res.ID,
			Provider: res.Provider, Outcome: "applied",
		})
	})
}
