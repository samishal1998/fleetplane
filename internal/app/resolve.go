package app

// Manual resolution of uncertain operations (ADR-API-001, plan R23):
// `uncertain` is a deliberate freeze — this is the operator's thaw.

import (
	"context"
	"encoding/json"

	"github.com/samishal1998/fleetplane/internal/phase"
	"github.com/samishal1998/fleetplane/internal/storage"
)

const (
	ResolveRetryVerification = "retry-verification"
	ResolveMarkFailed        = "mark-failed"
)

func (s *Service) ResolveOperation(ctx context.Context, id, action, actor string) error {
	opID := storage.OperationID(id)
	nowMs := s.clock.Now().UnixMilli()
	switch action {
	case ResolveRetryVerification:
		err := s.st.Tx(ctx, func(tx storage.TxStore) error {
			deadline := nowMs + IdemRetention.Milliseconds() // generous fresh window
			if err := tx.Operations().Transition(ctx, opID, storage.OpUncertain, storage.OpVerifying, func(o *storage.Operation) {
				o.NextAttemptAt = nil
				o.VerifyDeadlineAt = &deadline
			}); err != nil {
				return err
			}
			return tx.Events().Append(ctx, &storage.Event{
				TS: nowMs, Actor: actor, Type: "operation.resolve",
				OperationID: &opID, Outcome: "retry-verification",
			})
		})
		if err != nil {
			return err
		}
		s.engine.Kick()
		return nil
	case ResolveMarkFailed:
		return s.st.Tx(ctx, func(tx storage.TxStore) error {
			op, err := tx.Operations().Get(ctx, opID)
			if err != nil {
				return err
			}
			if err := tx.Operations().Transition(ctx, opID, storage.OpUncertain, storage.OpFailed, nil); err != nil {
				return err
			}
			if op.ResourceID != nil {
				fromPhase := phase.Provisioning
				if op.Kind == storage.OpKindDelete {
					fromPhase = phase.Deleting
				}
				_ = tx.Resources().CASPhase(ctx, *op.ResourceID, fromPhase, phase.Failed, nowMs)
			}
			return tx.Events().Append(ctx, &storage.Event{
				TS: nowMs, Actor: actor, Type: "operation.resolve",
				OperationID: &opID, Outcome: "mark-failed",
				Details: json.RawMessage(`{"note":"operator attested the provider-side outcome"}`),
			})
		})
	default:
		return invalid("unknown resolve action %q (want %s|%s)", action, ResolveRetryVerification, ResolveMarkFailed)
	}
}
