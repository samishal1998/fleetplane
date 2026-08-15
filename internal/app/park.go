package app

// Explicit operator park/start (docs/12 §4): POST /v1/resources/{id}:park
// and :start. Both are idempotent at the API level — repeating a request
// whose response was lost returns the current state instead of a conflict.

import (
	"context"
	"errors"

	"github.com/samishal1998/fleetplane/internal/phase"
	"github.com/samishal1998/fleetplane/internal/provision"
	"github.com/samishal1998/fleetplane/internal/storage"
)

// ErrAlreadyThere: the resource is already in (or moving to) the requested
// state — the API maps this to 200 with the current envelope.
var ErrAlreadyThere = errors.New("already in the requested state")

// ParkResource journals a stop for a ready machine. The operator's explicit
// intent bypasses the delete-protected gate (protection guards deletion;
// parking is reversible) and the queued-work gate.
func (s *Service) ParkResource(ctx context.Context, id, actor string) error {
	resID := storage.ResourceID(id)
	res, err := s.st.Resources().Get(ctx, resID)
	if err != nil {
		return err
	}
	if res.Phase == phase.Parking || res.Phase == phase.Parked {
		return ErrAlreadyThere
	}
	err = s.st.Tx(ctx, func(tx storage.TxStore) error {
		_, err := provision.JournalStop(ctx, tx, s.providers, resID, s.clock.Now().UnixMilli(), actor, false)
		return err
	})
	if err != nil {
		return err
	}
	s.engine.Kick()
	return nil
}

// StartResource journals a start for a parked machine.
func (s *Service) StartResource(ctx context.Context, id, actor string) error {
	resID := storage.ResourceID(id)
	res, err := s.st.Resources().Get(ctx, resID)
	if err != nil {
		return err
	}
	if res.Phase == phase.Starting || res.Phase == phase.Ready || res.Phase == phase.Allocated {
		return ErrAlreadyThere
	}
	err = s.st.Tx(ctx, func(tx storage.TxStore) error {
		_, err := provision.JournalStart(ctx, tx, s.providers, resID, s.clock.Now().UnixMilli(), actor)
		return err
	})
	if err != nil {
		return err
	}
	s.engine.Kick()
	return nil
}
