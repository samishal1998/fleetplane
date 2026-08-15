package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/samishal1998/fleetplane/internal/capacity"
	"github.com/samishal1998/fleetplane/internal/ids"
	"github.com/samishal1998/fleetplane/internal/lease"
	"github.com/samishal1998/fleetplane/internal/scheduler"
	"github.com/samishal1998/fleetplane/internal/storage"
)

// AttachScheduling wires the scheduler and lease manager (built after the
// Service because they share its dependencies).
func (s *Service) AttachScheduling(sched *scheduler.Scheduler, leases *lease.Manager) {
	s.sched = sched
	s.leases = leases
}

type AcquireCmd struct {
	Kind        string // default compute.machine
	Class       string
	Constraints json.RawMessage // {"cpu":{"min":2},...} (04 §4)
	Exclusive   bool
	Quantity    int
	TTL         time.Duration
	// MaxWait is the queue budget (docs/11 §8): how long the acquisition
	// may wait for existing/in-flight capacity before scaling up. Zero
	// falls back to the class default; the effective value is resolved
	// HERE, at accept time, and persisted — evaluation never re-consults
	// config, so restarts resume the same deadline.
	MaxWait time.Duration

	Actor       string
	IdemKey     string
	IdemScope   string
	RequestHash string

	BuildResponse func(*storage.Acquisition) (int, json.RawMessage)
}

// Acquire records the acquisition (the replayable side effect) and then
// satisfies it: reuse existing capacity first, else scale on demand (08 §3).
func (s *Service) Acquire(ctx context.Context, cmd AcquireCmd) (Outcome, error) {
	if cmd.Quantity == 0 {
		cmd.Quantity = 1
	}
	if cmd.Quantity != 1 {
		return Outcome{}, invalid("quantity must be 1 in v1 (batch acquisitions are a deferred ADR)")
	}
	if cmd.Kind == "" {
		cmd.Kind = "compute.machine"
	}
	if cmd.Class == "" && len(cmd.Constraints) == 0 {
		return Outcome{}, invalid("acquire needs a class and/or constraints")
	}

	nowMs := s.clock.Now().UnixMilli()
	acqID := storage.AcquisitionID(ids.New(ids.Acquisition))

	// Effective queue budget: request value, else the class default —
	// clamped (never rejected: rejection against a config-dependent limit
	// would break byte-identical idempotent replay) to leave the sweep
	// headroom below pendingTimeout.
	maxWait := cmd.MaxWait
	if maxWait <= 0 && cmd.Class != "" && s.sched != nil {
		if cls, ok := s.sched.Classes().Class(cmd.Class); ok {
			maxWait = cls.QueueMaxWait
		}
	}
	if pt := s.pendingTimeoutMs; pt > 0 && maxWait.Milliseconds() > pt-10_000 {
		maxWait = time.Duration(pt-10_000) * time.Millisecond
		if maxWait < 0 {
			maxWait = 0
		}
	}

	// The stored payload is the whole scheduling request (docs/11 §8):
	// exclusivity, constraints, and the resolved queue budget.
	constraints, err := json.Marshal(capacity.Request{
		Exclusive: cmd.Exclusive, Constraints: cmd.Constraints, MaxWaitMs: maxWait.Milliseconds(),
	})
	if err != nil {
		return Outcome{}, err
	}

	acq := &storage.Acquisition{
		ID: acqID, Actor: cmd.Actor, Kind: cmd.Kind, Class: cmd.Class,
		Constraints: constraints, Quantity: 1,
		TTLSeconds: int64(cmd.TTL / time.Second),
		State:      storage.AcqPending,
		CreatedAt:  nowMs, UpdatedAt: nowMs,
	}

	var out Outcome
	err = s.st.Tx(ctx, func(tx storage.TxStore) error {
		if cmd.IdemKey != "" {
			outcome, rec, err := tx.Idempotency().Begin(ctx, cmd.IdemScope, cmd.IdemKey, cmd.RequestHash, "")
			if err != nil {
				return err
			}
			switch outcome {
			case storage.BeginCompleted:
				out = Outcome{Status: rec.HTTPStatus, Body: rec.Result, Replayed: true}
				return nil
			case storage.BeginInProgress:
				return &InFlightError{}
			case storage.BeginMismatch:
				return ErrIdemMismatch
			}
		}
		if err := tx.Acquisitions().Insert(ctx, acq); err != nil {
			return err
		}
		if err := tx.Events().Append(ctx, &storage.Event{
			TS: nowMs, Actor: cmd.Actor, IdemKey: cmd.IdemKey, Type: "acquisition.requested",
			Intent: constraints, Outcome: "pending",
			Details: json.RawMessage(`{"acquisitionId":"` + string(acqID) + `"}`),
		}); err != nil {
			return err
		}
		status, body := cmd.BuildResponse(acq)
		out = Outcome{Status: status, Body: body}
		if cmd.IdemKey != "" {
			expires := nowMs + IdemRetention.Milliseconds()
			return tx.Idempotency().Complete(ctx, cmd.IdemScope, cmd.IdemKey, status, body, expires)
		}
		return nil
	})
	if err != nil || out.Replayed {
		return out, err
	}

	// Satisfaction is convergence, not part of the accept: reuse existing
	// capacity or pre-bind a create (05 §3/§5). No capacity and no class
	// to scale from = the acquisition fails.
	if _, err := s.sched.Satisfy(ctx, acqID); err != nil {
		if errors.Is(err, scheduler.ErrNoCapacity) {
			_ = s.st.Tx(ctx, func(tx storage.TxStore) error {
				return tx.Acquisitions().Transition(ctx, acqID, storage.AcqPending, storage.AcqFailed, s.clock.Now().UnixMilli())
			})
		} else {
			s.log.Error("acquire satisfaction", "acquisition_id", acqID, "error", err)
		}
	}
	return out, nil
}

func (s *Service) GetAcquisition(ctx context.Context, id string) (*storage.Acquisition, error) {
	acq, err := s.st.Acquisitions().Get(ctx, storage.AcquisitionID(id))
	if err != nil {
		return nil, err
	}
	// Bound acquisitions reach their resource via the lease; surface it on
	// the (otherwise cleared) pending field so the envelope carries it.
	if acq.State == storage.AcqBound && acq.LeaseID != nil && acq.PendingResourceID == nil {
		if lease, err := s.st.Leases().Get(ctx, *acq.LeaseID); err == nil {
			acq.PendingResourceID = &lease.ResourceID
		}
	}
	return acq, nil
}

// Release ends an acquisition (DELETE /v1/acquisitions/{id}). Releasing an
// already-released/expired acquisition is a no-op success — release must be
// safe to retry.
func (s *Service) Release(ctx context.Context, id, actor string) error {
	acq, err := s.st.Acquisitions().Get(ctx, storage.AcquisitionID(id))
	if err != nil {
		return err
	}
	if acq.State == storage.AcqReleased || acq.State == storage.AcqExpired {
		return nil
	}
	if err := s.leases.ReleaseAcquisition(ctx, acq.ID); err != nil {
		return err
	}
	return nil
}
