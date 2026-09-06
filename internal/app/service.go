package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/samishal1998/fleetplane/internal/ids"
	"github.com/samishal1998/fleetplane/internal/lease"
	"github.com/samishal1998/fleetplane/internal/operations"
	"github.com/samishal1998/fleetplane/internal/phase"
	"github.com/samishal1998/fleetplane/internal/provision"
	"github.com/samishal1998/fleetplane/internal/reconcile"
	"github.com/samishal1998/fleetplane/internal/scheduler"
	"github.com/samishal1998/fleetplane/internal/storage"
	"github.com/samishal1998/fleetplane/pkg/kinds"
	"github.com/samishal1998/fleetplane/pkg/sdk"
	"github.com/samishal1998/fleetplane/pkg/sdk/provider"
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

// overlayJSON lays the top-level keys of over onto base (shallow: a key in
// over replaces the base value wholesale). Empty over returns base.
func overlayJSON(base, over json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(over)) == 0 || string(bytes.TrimSpace(over)) == "null" {
		return base, nil
	}
	var b, o map[string]json.RawMessage
	if err := json.Unmarshal(base, &b); err != nil {
		return nil, fmt.Errorf("class spec: %w", err)
	}
	if err := json.Unmarshal(over, &o); err != nil {
		return nil, fmt.Errorf("override must be a JSON object: %w", err)
	}
	if b == nil {
		b = map[string]json.RawMessage{}
	}
	for k, v := range o {
		b[k] = v
	}
	return json.Marshal(b)
}

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

	sched  *scheduler.Scheduler
	leases *lease.Manager
	rec    *reconcile.Reconciler

	// pendingTimeoutMs caps every queue deadline (docs/11 §8; the clamp
	// replaces request-time rejection so idempotent replays stay valid).
	pendingTimeoutMs int64
}

// SetPendingTimeout wires the effective acquire.pendingTimeout (boot).
func (s *Service) SetPendingTimeout(ms int64) { s.pendingTimeoutMs = ms }

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
	// Class is optional: kind/provider default from it and Spec is a
	// shallow overlay on the class spec (top-level keys replace wholesale).
	Class  string
	Spec   json.RawMessage // kind-specific (compute.MachineSpec)
	Labels map[string]string

	Actor       string
	IdemKey     string // optional
	IdemScope   string
	RequestHash string

	BuildResponse BuildResponse
}

// CreateResource journals a create (TxA) and returns immediately; the
// operation engine converges the resource to ready asynchronously.
func (s *Service) CreateResource(ctx context.Context, cmd CreateResourceCmd) (Outcome, error) {
	if cmd.Class != "" {
		rec, err := s.st.Classes().Get(ctx, cmd.Class)
		if errors.Is(err, storage.ErrNotFound) {
			return Outcome{}, invalid("unknown class %q", cmd.Class)
		} else if err != nil {
			return Outcome{}, err
		}
		if cmd.Kind != "" && cmd.Kind != rec.Kind {
			return Outcome{}, invalid("kind %q conflicts with class %q (kind %s)", cmd.Kind, cmd.Class, rec.Kind)
		}
		if cmd.Provider != "" && cmd.Provider != string(rec.Provider) {
			return Outcome{}, invalid("provider %q conflicts with class %q (provider %s)", cmd.Provider, cmd.Class, rec.Provider)
		}
		cmd.Kind, cmd.Provider = rec.Kind, string(rec.Provider)
		merged, err := overlayJSON(rec.Spec, cmd.Spec)
		if err != nil {
			return Outcome{}, invalid("spec overlay: %s", err)
		}
		cmd.Spec = merged
	}
	kind := provider.ResourceKind(cmd.Kind)
	if err := kinds.Validate(kind, cmd.Spec); err != nil {
		return Outcome{}, &ValidationError{Msg: err.Error()}
	}
	inst, ok := s.providers.Instance(storage.ProviderInstance(cmd.Provider))
	if !ok {
		return Outcome{}, invalid("unknown provider instance %q (configured: %v)", cmd.Provider, s.providers.Names())
	}
	if _, ok := inst.ResourceDriver(kind); !ok {
		return Outcome{}, invalid("provider %q does not drive %s", cmd.Provider, kind)
	}

	nowMs := s.clock.Now().UnixMilli()
	prepared, err := provision.Prepare(ctx, s.providers, s.ownerID, provision.CreateSpec{
		Kind: cmd.Kind, Provider: cmd.Provider, Name: cmd.Name,
		Spec: cmd.Spec, Labels: cmd.Labels, Class: cmd.Class,
	}, nowMs)
	if err != nil {
		return Outcome{}, err
	}
	if cmd.IdemKey != "" {
		prepared.Op.IdemScope, prepared.Op.IdemKey = &cmd.IdemScope, &cmd.IdemKey
	}

	var out Outcome
	err = s.st.Tx(ctx, func(tx storage.TxStore) error {
		if cmd.IdemKey != "" {
			outcome, rec, err := tx.Idempotency().Begin(ctx, cmd.IdemScope, cmd.IdemKey, cmd.RequestHash, prepared.Op.ID)
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
		if err := prepared.Journal(ctx, tx, cmd.Actor, cmd.IdemKey); err != nil {
			return err
		}
		status, body := cmd.BuildResponse(prepared.Resource)
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

// deleteGates are the shared deletion safeguards (invariants 3 and 5).
func deleteGates(ctx context.Context, tx storage.TxStore, res *storage.Resource) error {
	if res.DeletedAt != nil {
		return fmt.Errorf("%w: resource already deleted", storage.ErrNotFound)
	}
	if res.Ownership != storage.OwnershipManaged {
		return invalid("resource %s has ownership %q; only managed resources are deletable in v1 (invariant 5)", res.ID, res.Ownership)
	}
	if res.DeleteProtected {
		return fmt.Errorf("%w: resource %s is protected", storage.ErrConflict, res.ID)
	}
	if n, err := tx.Leases().CountActive(ctx, res.ID); err != nil {
		return err
	} else if n > 0 {
		return fmt.Errorf("%w: resource %s has %d active lease(s) (invariant 3)", storage.ErrConflict, res.ID, n)
	}
	return nil
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
	// DryRun runs every deletion gate and reports the plan without
	// mutating anything ("plan before mutate", 01 §6).
	DryRun bool

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

	if cmd.DryRun {
		var out Outcome
		err := s.st.View(ctx, func(tx storage.TxStore) error {
			res, err := tx.Resources().Get(ctx, resID)
			if err != nil {
				return err
			}
			if err := deleteGates(ctx, tx, res); err != nil {
				return err
			}
			status, body := cmd.BuildResponse(res)
			out = Outcome{Status: status, Body: body}
			return nil
		})
		return out, err
	}

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
		if err := deleteGates(ctx, tx, res); err != nil {
			return err
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
