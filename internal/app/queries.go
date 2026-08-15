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
