package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/samimishal/fleetplane/internal/ids"
	"github.com/samimishal/fleetplane/internal/operations"
	"github.com/samimishal/fleetplane/internal/phase"
	"github.com/samimishal/fleetplane/internal/storage"
	"github.com/samimishal/fleetplane/pkg/kinds/compute"
	"github.com/samimishal/fleetplane/pkg/sdk"
	"github.com/samimishal/fleetplane/pkg/sdk/provider"
)

// Typed errors the API layer maps to HTTP.
var ErrIdemMismatch = errors.New("idempotency key reused with a different request")

// InFlightError: the idempotency key's original request is still running —
// callers poll the operation instead of retrying (invariant 7 surface).
type InFlightError struct{ OperationID storage.OperationID }

func (e *InFlightError) Error() string {
	return fmt.Sprintf("request in flight (operation %s)", e.OperationID)
}

// ValidationError maps to 400.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

// IdemRetention is how long completed idempotency outcomes replay (R17).
const IdemRetention = 48 * time.Hour

type Service struct {
	st        storage.Store
	providers *Providers
	engine    *operations.Engine
	clock     sdk.Clock
	log       *slog.Logger
	ownerID   string
}

func NewService(st storage.Store, p *Providers, e *operations.Engine, clock sdk.Clock, log *slog.Logger, ownerID string) *Service {
	return &Service{st: st, providers: p, engine: e, clock: clock, log: log, ownerID: ownerID}
}

// Outcome is a keyed command's result: the exact bytes the API returns.
// Replayed outcomes are the ORIGINAL stored bytes (invariant 2).
type Outcome struct {
	Status   int
	Body     json.RawMessage
	Replayed bool
}

// BuildResponse renders the API response for a freshly created record; it
// runs inside the command transaction so the stored idempotency outcome and
// the side effect commit together (plan R17).
type BuildResponse func(*storage.Resource) (int, json.RawMessage)

type CreateResourceCmd struct {
	Kind     string
	Provider string
	Name     string
	Spec     json.RawMessage // kind-specific (compute.MachineSpec)
	Labels   map[string]string

	Actor       string
	IdemKey     string // optional
	IdemScope   string
	RequestHash string

	BuildResponse BuildResponse
}

// CreateResource journals a create (TxA) and returns immediately; the
// operation engine converges the resource to ready asynchronously.
func (s *Service) CreateResource(ctx context.Context, cmd CreateResourceCmd) (Outcome, error) {
	if cmd.Kind != string(compute.Kind) {
		return Outcome{}, invalid("unknown resource kind %q", cmd.Kind)
	}
	inst, ok := s.providers.Instance(storage.ProviderInstance(cmd.Provider))
	if !ok {
		return Outcome{}, invalid("unknown provider instance %q (configured: %v)", cmd.Provider, s.providers.Names())
	}
	if _, err := compute.ParseSpec(cmd.Spec); err != nil {
		return Outcome{}, &ValidationError{Msg: err.Error()}
	}
	driver, ok := inst.ResourceDriver(compute.Kind)
	if !ok {
		return Outcome{}, invalid("provider %q does not drive %s", cmd.Provider, compute.Kind)
	}

	resID := storage.ResourceID(ids.New(ids.Resource))
	opID := storage.OperationID(ids.New(ids.Operation))
	name := cmd.Name
	if name == "" {
		name = string(resID)
	}

	// Plan is pure (03 §2); the kernel composes identity labels (R2) and
	// stamps ActionID := OperationID after planning.
	plan, err := driver.Plan(ctx, provider.PlanRequest{
		ResourceID: string(resID),
		Desired: &provider.DesiredState{
			Name:   name,
			Spec:   cmd.Spec,
			Labels: provider.IdentityLabels(s.ownerID, string(resID), string(opID)),
		},
	})
	if err != nil {
		return Outcome{}, err
	}
	if len(plan.Actions) != 1 {
		return Outcome{}, fmt.Errorf("driver planned %d actions for a create, want 1", len(plan.Actions))
	}
	action := plan.Actions[0]
	action.ActionID = string(opID)
	actionJSON, err := json.Marshal(action)
	if err != nil {
		return Outcome{}, err
	}

	nowMs := s.clock.Now().UnixMilli()
	var out Outcome
	err = s.st.Tx(ctx, func(tx storage.TxStore) error {
		if cmd.IdemKey != "" {
			outcome, rec, err := tx.Idempotency().Begin(ctx, cmd.IdemScope, cmd.IdemKey, cmd.RequestHash, opID)
			if err != nil {
				return err
			}
			switch outcome {
			case storage.BeginCompleted:
				out = Outcome{Status: rec.HTTPStatus, Body: rec.Result, Replayed: true}
				return nil
			case storage.BeginInProgress:
				var opPtr storage.OperationID
				if rec.OperationID != nil {
					opPtr = *rec.OperationID
				}
				return &InFlightError{OperationID: opPtr}
			case storage.BeginMismatch:
				return ErrIdemMismatch
			}
		}

		res := &storage.Resource{
			ID: resID, Name: name, Kind: cmd.Kind,
			Provider:  storage.ProviderInstance(cmd.Provider),
			Ownership: storage.OwnershipManaged, Phase: phase.Provisioning,
			Spec: cmd.Spec, Labels: cmd.Labels,
			CreatedAt: nowMs, UpdatedAt: nowMs,
		}
		if err := tx.Resources().Create(ctx, res); err != nil {
			return err
		}
		op := &storage.Operation{
			ID: opID, Kind: storage.OpKindCreate, ResourceID: &resID,
			Provider: storage.ProviderInstance(cmd.Provider),
			Action:   actionJSON, State: storage.OpJournaled,
			CreatedAt: nowMs, UpdatedAt: nowMs,
		}
		if cmd.IdemKey != "" {
			op.IdemScope, op.IdemKey = &cmd.IdemScope, &cmd.IdemKey
		}
		if err := tx.Operations().Append(ctx, op); err != nil {
			return err
		}
		if err := tx.Events().Append(ctx, &storage.Event{
			TS: nowMs, Actor: cmd.Actor, IdemKey: cmd.IdemKey, Type: "resource.create",
			ResourceID: &resID, OperationID: &opID,
			Provider: storage.ProviderInstance(cmd.Provider),
			Intent:   cmd.Spec, Outcome: "journaled",
		}); err != nil {
			return err
		}
		status, body := cmd.BuildResponse(res)
		out = Outcome{Status: status, Body: body}
		if cmd.IdemKey != "" {
			// The journaled intent IS the side effect of an async-accept
			// API; the stored bytes replay it (invariant 2).
			expires := nowMs + IdemRetention.Milliseconds()
			return tx.Idempotency().Complete(ctx, cmd.IdemScope, cmd.IdemKey, status, body, expires)
		}
		return nil
	})
	if err != nil {
		return Outcome{}, err
	}
	if !out.Replayed {
		s.engine.Kick()
	}
	return out, nil
}

func (s *Service) GetResource(ctx context.Context, id string) (*storage.Resource, error) {
	return s.st.Resources().Get(ctx, storage.ResourceID(id))
}

func (s *Service) ListResources(ctx context.Context, f storage.ResourceFilter) ([]*storage.Resource, error) {
	return s.st.Resources().List(ctx, f)
}

type DeleteResourceCmd struct {
	ID    string
	Actor string

	IdemKey     string
	IdemScope   string
	RequestHash string

	BuildResponse BuildResponse
}

// DeleteResource journals a provider delete after the deletion gates
// (invariants 3 and 5) pass INSIDE the transaction.
func (s *Service) DeleteResource(ctx context.Context, cmd DeleteResourceCmd) (Outcome, error) {
	resID := storage.ResourceID(cmd.ID)
	opID := storage.OperationID(ids.New(ids.Operation))
	nowMs := s.clock.Now().UnixMilli()

	var out Outcome
	err := s.st.Tx(ctx, func(tx storage.TxStore) error {
		if cmd.IdemKey != "" {
			outcome, rec, err := tx.Idempotency().Begin(ctx, cmd.IdemScope, cmd.IdemKey, cmd.RequestHash, opID)
			if err != nil {
				return err
			}
			switch outcome {
			case storage.BeginCompleted:
				out = Outcome{Status: rec.HTTPStatus, Body: rec.Result, Replayed: true}
				return nil
			case storage.BeginInProgress:
				var opPtr storage.OperationID
				if rec.OperationID != nil {
					opPtr = *rec.OperationID
				}
				return &InFlightError{OperationID: opPtr}
			case storage.BeginMismatch:
				return ErrIdemMismatch
			}
		}

		res, err := tx.Resources().Get(ctx, resID)
		if err != nil {
			return err
		}
		// Deletion gates (07 §5, invariants 3/5) — checked in the same tx
		// that journals the delete, so they cannot race.
		if res.DeletedAt != nil {
			return fmt.Errorf("%w: resource already deleted", storage.ErrNotFound)
		}
		if res.Ownership != storage.OwnershipManaged {
			return invalid("resource %s has ownership %q; only managed resources are deletable in v1 (invariant 5)", res.ID, res.Ownership)
		}
		if res.DeleteProtected {
			return fmt.Errorf("%w: resource %s is protected", storage.ErrConflict, res.ID)
		}
		if n, err := tx.Leases().CountActive(ctx, resID); err != nil {
			return err
		} else if n > 0 {
			return fmt.Errorf("%w: resource %s has %d active lease(s) (invariant 3)", storage.ErrConflict, res.ID, n)
		}

		if res.ExternalID == nil {
			// Never materialized at the provider and no active op holds
			// the slot: tombstone directly. (If an op IS active, Append
			// below fails with ErrConflict — the safe answer.)
			if err := tx.Resources().MarkDeleted(ctx, resID, nowMs); err != nil {
				return err
			}
			status, body := cmd.BuildResponse(res)
			out = Outcome{Status: status, Body: body}
			if cmd.IdemKey != "" {
				expires := nowMs + IdemRetention.Milliseconds()
				return tx.Idempotency().Complete(ctx, cmd.IdemScope, cmd.IdemKey, status, body, expires)
			}
			return nil
		}

		if !phase.CanTransition(res.Phase, phase.Deleting) {
			return fmt.Errorf("%w: resource %s in phase %s cannot be deleted directly", storage.ErrConflict, res.ID, res.Phase)
		}
		if err := tx.Resources().CASPhase(ctx, resID, res.Phase, phase.Deleting, nowMs); err != nil {
			return err
		}

		var ref provider.ExternalRef
		if len(res.ExternalRef) > 0 {
			_ = json.Unmarshal(res.ExternalRef, &ref)
		}
		if ref.ID == "" {
			ref.ID = *res.ExternalID
		}
		action := provider.Action{
			ActionID: string(opID), Kind: "delete", ResourceID: string(resID),
			Ref: &ref, Destructive: true,
		}
		actionJSON, _ := json.Marshal(action)
		op := &storage.Operation{
			ID: opID, Kind: storage.OpKindDelete, ResourceID: &resID,
			Provider: res.Provider, Action: actionJSON, State: storage.OpJournaled,
			CreatedAt: nowMs, UpdatedAt: nowMs,
		}
		if cmd.IdemKey != "" {
			op.IdemScope, op.IdemKey = &cmd.IdemScope, &cmd.IdemKey
		}
		if err := tx.Operations().Append(ctx, op); err != nil {
			return err
		}
		if err := tx.Events().Append(ctx, &storage.Event{
			TS: nowMs, Actor: cmd.Actor, IdemKey: cmd.IdemKey, Type: "resource.delete",
			ResourceID: &resID, OperationID: &opID, Provider: res.Provider, Outcome: "journaled",
		}); err != nil {
			return err
		}
		status, body := cmd.BuildResponse(res)
		out = Outcome{Status: status, Body: body}
		if cmd.IdemKey != "" {
			expires := nowMs + IdemRetention.Milliseconds()
			return tx.Idempotency().Complete(ctx, cmd.IdemScope, cmd.IdemKey, status, body, expires)
		}
		return nil
	})
	if err != nil {
		return Outcome{}, err
	}
	if !out.Replayed {
		s.engine.Kick()
	}
	return out, nil
}
