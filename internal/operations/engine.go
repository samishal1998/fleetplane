// Package operations is the operation engine: the only component that calls
// provider mutations, always through the journal protocol (06 §4, ADR-017):
//
//	TxA (owned by the app layer): journal `journaled` + idempotency + event
//	TxB: journaled → in_flight (attempt++, verify deadline)   — then, only then:
//	     provider Apply (labels carry fleetplane.io/op = operation ID)
//	TxC: in_flight → external_accepted (accept-time ref persisted, 05 §10)
//	poll ObserveOperation until terminal
//	TxD: → succeeded + resource phase/tombstone + event
//
// Crash windows: `journaled` = provably never called → safe re-dispatch;
// `in_flight` = unknown → `verifying` → discover by op label within the
// verify window; found → adopt; confirmed absent → exactly one re-dispatch
// with the SAME operation ID (invariant 7).
package operations

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/samimishal/fleetplane/internal/phase"
	"github.com/samimishal/fleetplane/internal/storage"
	"github.com/samimishal/fleetplane/pkg/sdk"
	"github.com/samimishal/fleetplane/pkg/sdk/provider"
)

// Providers resolves a configured provider instance.
type Providers interface {
	Instance(name storage.ProviderInstance) (provider.Provider, bool)
}

type Config struct {
	PollInterval time.Duration // engine wake-up cadence (fallback; Kick is primary)
	VerifyWindow time.Duration // uncertain-create resolution window (plan R10)
	BackoffBase  time.Duration // ADR-014
	BackoffCap   time.Duration
	MaxBatch     int
}

func (c *Config) defaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.VerifyWindow <= 0 {
		c.VerifyWindow = 120 * time.Second
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = 2 * time.Second
	}
	if c.BackoffCap <= 0 {
		c.BackoffCap = 5 * time.Minute
	}
	if c.MaxBatch <= 0 {
		c.MaxBatch = 16
	}
}

// Hooks are test failpoints (crash-matrix harness); nil in production.
type Hooks struct {
	AfterInFlight func(storage.OperationID) // after TxB commit, before Apply
	AfterApply    func(storage.OperationID) // after Apply returns, before TxC
}

type Engine struct {
	st        storage.Store
	providers Providers
	clock     sdk.Clock
	log       *slog.Logger
	cfg       Config
	hooks     Hooks

	kick chan struct{}
	// OnTerminal is invoked (if set) after an operation reaches a terminal
	// state — the reconciler's level-trigger (05 §8).
	OnTerminal func(op *storage.Operation)
}

func New(st storage.Store, providers Providers, clock sdk.Clock, log *slog.Logger, cfg Config, hooks Hooks) *Engine {
	cfg.defaults()
	return &Engine{
		st: st, providers: providers, clock: clock, log: log, cfg: cfg, hooks: hooks,
		kick: make(chan struct{}, 1),
	}
}

// Kick wakes the engine loop (level-triggered; coalesces).
func (e *Engine) Kick() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// Resume routes every non-terminal operation after a restart. It MUST
// complete before the API accepts mutations (plan R11).
func (e *Engine) Resume(ctx context.Context) error {
	ops, err := e.st.Operations().NonTerminal(ctx)
	if err != nil {
		return fmt.Errorf("resume scan: %w", err)
	}
	for _, op := range ops {
		switch op.State {
		case storage.OpInFlight:
			// Crash inside the uncertain window: verify before any
			// re-mutation (invariant 7).
			err := e.st.Tx(ctx, func(tx storage.TxStore) error {
				return tx.Operations().Transition(ctx, op.ID, storage.OpInFlight, storage.OpVerifying, func(o *storage.Operation) {
					o.NextAttemptAt = nil
				})
			})
			if err != nil {
				return fmt.Errorf("resume op %s: %w", op.ID, err)
			}
			e.log.Warn("operation resumed into verification", "operation_id", op.ID)
		case storage.OpUncertain:
			e.log.Error("operation frozen uncertain; use operations :resolve", "operation_id", op.ID)
		default:
			// journaled / external_accepted / verifying: Due picks them up.
		}
	}
	e.Kick()
	return nil
}

// Run drives operations until ctx is done.
func (e *Engine) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.kick:
		case <-e.clock.After(e.cfg.PollInterval):
		}
		for {
			n, err := e.Step(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				e.log.Error("engine step", "error", err)
				break
			}
			if n == 0 {
				break
			}
		}
	}
}

// Step drives one batch of due operations; returns how many it touched.
func (e *Engine) Step(ctx context.Context) (int, error) {
	nowMs := e.clock.Now().UnixMilli()
	due, err := e.st.Operations().Due(ctx, nowMs, e.cfg.MaxBatch)
	if err != nil {
		return 0, err
	}
	for _, op := range due {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		e.drive(ctx, op)
	}
	return len(due), nil
}

func (e *Engine) drive(ctx context.Context, op *storage.Operation) {
	var err error
	switch op.State {
	case storage.OpJournaled:
		err = e.dispatch(ctx, op)
	case storage.OpExternalAccepted:
		err = e.poll(ctx, op)
	case storage.OpVerifying:
		err = e.verify(ctx, op)
	default:
		return
	}
	if err != nil && ctx.Err() == nil {
		e.log.Error("operation drive", "operation_id", op.ID, "state", op.State, "error", err)
	}
}

// --- dispatch: TxB → Apply → TxC ---

func (e *Engine) dispatch(ctx context.Context, op *storage.Operation) error {
	driver, action, err := e.driverAndAction(ctx, op)
	if err != nil {
		return e.failOp(ctx, op.ID, op.State, err)
	}

	nowMs := e.clock.Now().UnixMilli()
	deadline := nowMs + e.cfg.VerifyWindow.Milliseconds()
	err = e.st.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Operations().Transition(ctx, op.ID, storage.OpJournaled, storage.OpInFlight, func(o *storage.Operation) {
			o.Attempt++
			o.VerifyDeadlineAt = &deadline
			o.NextAttemptAt = nil
		})
	})
	if err != nil {
		return err // raced; another path owns it
	}
	if e.hooks.AfterInFlight != nil {
		e.hooks.AfterInFlight(op.ID)
	}

	opRef, applyErr := driver.Apply(ctx, action)
	if e.hooks.AfterApply != nil {
		e.hooks.AfterApply(op.ID)
	}
	if applyErr != nil {
		return e.handleApplyError(ctx, op, applyErr)
	}

	refJSON, err := json.Marshal(opRef)
	if err != nil {
		return err
	}
	var extRef json.RawMessage
	if opRef.Ref != nil {
		extRef, _ = json.Marshal(opRef.Ref)
	}
	// TxC: the accept-time ref is preserved BEFORE any listability
	// assumption (05 §10).
	err = e.st.Tx(ctx, func(tx storage.TxStore) error {
		if err := tx.Operations().Transition(ctx, op.ID, storage.OpInFlight, storage.OpExternalAccepted, func(o *storage.Operation) {
			o.ExternalOp = refJSON
			o.ExternalRef = extRef
			o.NextAttemptAt = nil
		}); err != nil {
			return err
		}
		if op.Kind == storage.OpKindCreate && op.ResourceID != nil && opRef.Ref != nil {
			if err := tx.Resources().SetExternalRef(ctx, *op.ResourceID, opRef.Ref.ID, extRef); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	e.Kick() // poll promptly
	return nil
}

func (e *Engine) handleApplyError(ctx context.Context, op *storage.Operation, applyErr error) error {
	class := provider.Classify(applyErr)
	nowMs := e.clock.Now().UnixMilli()

	// not_found on delete is success (docs/03 §6, FI-7).
	if op.Kind == storage.OpKindDelete && class == provider.ErrNotFound {
		return e.succeedDelete(ctx, op, storage.OpInFlight)
	}

	switch {
	case class == provider.ErrInvalid || class == provider.ErrTerminal:
		return e.failOp(ctx, op.ID, storage.OpInFlight, applyErr)
	case provider.Effect(applyErr) == provider.EffectNone:
		// Provably never reached the provider: plain journaled retry.
		delay := e.backoff(op.Attempt, applyErr)
		next := nowMs + delay.Milliseconds()
		return e.st.Tx(ctx, func(tx storage.TxStore) error {
			return tx.Operations().Transition(ctx, op.ID, storage.OpInFlight, storage.OpJournaled, func(o *storage.Operation) {
				o.NextAttemptAt = &next
				setErr(o, class, applyErr)
			})
		})
	default:
		// EffectMaybe: outcome unknown → verify before any re-mutation.
		next := nowMs + 2000
		return e.st.Tx(ctx, func(tx storage.TxStore) error {
			return tx.Operations().Transition(ctx, op.ID, storage.OpInFlight, storage.OpVerifying, func(o *storage.Operation) {
				o.NextAttemptAt = &next
				setErr(o, class, applyErr)
			})
		})
	}
}

// --- poll: ObserveOperation → TxD ---

func (e *Engine) poll(ctx context.Context, op *storage.Operation) error {
	driver, _, err := e.driverAndAction(ctx, op)
	if err != nil {
		return e.failOp(ctx, op.ID, op.State, err)
	}
	var opRef provider.OperationRef
	if err := json.Unmarshal(op.ExternalOp, &opRef); err != nil {
		return e.failOp(ctx, op.ID, op.State, fmt.Errorf("corrupt external op ref: %w", err))
	}

	status, obsErr := driver.ObserveOperation(ctx, opRef)
	if obsErr != nil {
		if provider.Classify(obsErr) == provider.ErrNotFound {
			if op.Kind == storage.OpKindDelete {
				return e.succeedDelete(ctx, op, storage.OpExternalAccepted)
			}
			// A create whose object vanished mid-poll: resolve via the
			// verification procedure — never blind-succeed or blind-retry.
			return e.st.Tx(ctx, func(tx storage.TxStore) error {
				return tx.Operations().Transition(ctx, op.ID, storage.OpExternalAccepted, storage.OpVerifying, func(o *storage.Operation) {
					o.NextAttemptAt = nil
				})
			})
		}
		return e.reschedule(ctx, op, obsErr)
	}

	switch status.State {
	case provider.OpSucceeded:
		if op.Kind == storage.OpKindDelete {
			return e.succeedDelete(ctx, op, storage.OpExternalAccepted)
		}
		return e.succeedCreate(ctx, op, status)
	case provider.OpFailed:
		var cause error = status.Failure
		if status.Failure == nil {
			cause = fmt.Errorf("provider reported failure without detail")
		}
		return e.failOp(ctx, op.ID, storage.OpExternalAccepted, cause)
	default: // pending / running / unknown → keep polling
		return e.reschedule(ctx, op, nil)
	}
}

func (e *Engine) succeedCreate(ctx context.Context, op *storage.Operation, status provider.OperationStatus) error {
	nowMs := e.clock.Now().UnixMilli()
	err := e.st.Tx(ctx, func(tx storage.TxStore) error {
		if err := tx.Operations().Transition(ctx, op.ID, op.State, storage.OpSucceeded, nil); err != nil {
			return err
		}
		if op.ResourceID != nil {
			if status.Resource != nil {
				raw, _ := json.Marshal(status.Resource)
				_ = tx.Resources().PutObserved(ctx, &storage.ObservedSnapshot{
					ResourceID: *op.ResourceID, ObservedAt: nowMs,
					ProviderPhase: string(status.Resource.Phase), Raw: raw,
				})
				capJSON, _ := json.Marshal(status.Resource.Capacity)
				if err := tx.Resources().SetProviderFacts(ctx, *op.ResourceID,
					status.Resource.Extensions, capJSON, nowMs); err != nil {
					return err
				}
			}
			// provisioning → ready (readiness probes gate here later, R16).
			if err := tx.Resources().CASPhase(ctx, *op.ResourceID, phase.Provisioning, phase.Ready, nowMs); err != nil {
				return err
			}
		}
		return tx.Events().Append(ctx, &storage.Event{
			TS: nowMs, Type: "operation.succeeded", OperationID: &op.ID,
			ResourceID: op.ResourceID, Provider: op.Provider, Outcome: "succeeded",
		})
	})
	if err != nil {
		return err
	}
	e.notifyTerminal(ctx, op.ID)
	return nil
}

func (e *Engine) succeedDelete(ctx context.Context, op *storage.Operation, from storage.OpState) error {
	nowMs := e.clock.Now().UnixMilli()
	err := e.st.Tx(ctx, func(tx storage.TxStore) error {
		if err := tx.Operations().Transition(ctx, op.ID, from, storage.OpSucceeded, nil); err != nil {
			return err
		}
		if op.ResourceID != nil {
			// Deletion terminality is the tombstone (ADR-017).
			if err := tx.Resources().MarkDeleted(ctx, *op.ResourceID, nowMs); err != nil {
				return err
			}
		}
		return tx.Events().Append(ctx, &storage.Event{
			TS: nowMs, Type: "operation.succeeded", OperationID: &op.ID,
			ResourceID: op.ResourceID, Provider: op.Provider, Outcome: "succeeded",
		})
	})
	if err != nil {
		return err
	}
	e.notifyTerminal(ctx, op.ID)
	return nil
}

// --- verify: the invariant-7 resolution procedure (plan R10) ---

func (e *Engine) verify(ctx context.Context, op *storage.Operation) error {
	driver, action, err := e.driverAndAction(ctx, op)
	if err != nil {
		return e.failOp(ctx, op.ID, op.State, err)
	}
	nowMs := e.clock.Now().UnixMilli()

	if op.Kind == storage.OpKindDelete {
		// Deletes verify via Get: absent ⇒ succeeded; present ⇒ retry.
		if action.Ref == nil {
			return e.failOp(ctx, op.ID, storage.OpVerifying, fmt.Errorf("delete op without ref"))
		}
		_, getErr := driver.Get(ctx, *action.Ref)
		if provider.IsClass(getErr, provider.ErrNotFound) {
			return e.succeedDelete(ctx, op, storage.OpVerifying)
		}
		if getErr != nil {
			return e.reschedule(ctx, op, getErr)
		}
		return e.st.Tx(ctx, func(tx storage.TxStore) error {
			return tx.Operations().Transition(ctx, op.ID, storage.OpVerifying, storage.OpJournaled, nil)
		})
	}

	// Creates: discover by the op label (the create-dedup anchor, R2).
	desc := e.descriptor(op)
	if !desc.SupportsLabelDiscovery {
		e.log.Error("driver cannot discover by label; freezing uncertain", "operation_id", op.ID)
		return e.st.Tx(ctx, func(tx storage.TxStore) error {
			return tx.Operations().Transition(ctx, op.ID, storage.OpVerifying, storage.OpUncertain, nil)
		})
	}
	found, err := driver.Discover(ctx, provider.DiscoverRequest{
		Scope:    provider.ScopeOwned,
		Selector: map[string]string{provider.LabelOp: string(op.ID)},
	})
	if err != nil {
		return e.reschedule(ctx, op, err)
	}
	if len(found) > 0 {
		obs := found[0]
		refJSON, _ := json.Marshal(provider.OperationRef{ActionID: string(op.ID), Ref: &obs.Ref})
		extRef, _ := json.Marshal(obs.Ref)
		err := e.st.Tx(ctx, func(tx storage.TxStore) error {
			if err := tx.Operations().Transition(ctx, op.ID, storage.OpVerifying, storage.OpExternalAccepted, func(o *storage.Operation) {
				o.ExternalOp = refJSON
				o.ExternalRef = extRef
				o.NextAttemptAt = nil
			}); err != nil {
				return err
			}
			if op.ResourceID != nil {
				return tx.Resources().SetExternalRef(ctx, *op.ResourceID, obs.Ref.ID, extRef)
			}
			return nil
		})
		if err != nil {
			return err
		}
		e.log.Info("uncertain create adopted by op label", "operation_id", op.ID, "external_id", obs.Ref.ID)
		e.Kick()
		return nil
	}

	if op.VerifyDeadlineAt != nil && nowMs < *op.VerifyDeadlineAt {
		// Inside the consistency window: keep looking (05 §10 forbids
		// trusting a single list call).
		return e.reschedule(ctx, op, nil)
	}
	// Confirmed absent past the window: exactly one re-dispatch with the
	// SAME operation ID; a late ghost still carries our op label and is
	// collapsed by discovery (plan R8).
	e.log.Warn("uncertain create confirmed absent; re-dispatching once", "operation_id", op.ID)
	return e.st.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Operations().Transition(ctx, op.ID, storage.OpVerifying, storage.OpJournaled, func(o *storage.Operation) {
			o.NextAttemptAt = nil
		})
	})
}

// --- helpers ---

func (e *Engine) driverAndAction(ctx context.Context, op *storage.Operation) (provider.ResourceDriver, provider.Action, error) {
	var action provider.Action
	if err := json.Unmarshal(op.Action, &action); err != nil {
		return nil, action, fmt.Errorf("corrupt journaled action: %w", err)
	}
	p, ok := e.providers.Instance(op.Provider)
	if !ok {
		return nil, action, fmt.Errorf("provider instance %q not configured", op.Provider)
	}
	kind := provider.ResourceKind("compute.machine")
	if op.ResourceID != nil {
		if r, err := e.st.Resources().Get(ctx, *op.ResourceID); err == nil {
			kind = provider.ResourceKind(r.Kind)
		}
	}
	driver, ok := p.ResourceDriver(kind)
	if !ok {
		return nil, action, fmt.Errorf("provider %q has no driver for kind %q", op.Provider, kind)
	}
	return driver, action, nil
}

func (e *Engine) descriptor(op *storage.Operation) provider.Descriptor {
	if p, ok := e.providers.Instance(op.Provider); ok {
		return p.Descriptor()
	}
	return provider.Descriptor{}
}

func (e *Engine) failOp(ctx context.Context, id storage.OperationID, from storage.OpState, cause error) error {
	nowMs := e.clock.Now().UnixMilli()
	op, err := e.st.Operations().Get(ctx, id)
	if err != nil {
		return err
	}
	err = e.st.Tx(ctx, func(tx storage.TxStore) error {
		if err := tx.Operations().Transition(ctx, id, from, storage.OpFailed, func(o *storage.Operation) {
			setErr(o, provider.Classify(cause), cause)
		}); err != nil {
			return err
		}
		if op.ResourceID != nil {
			fromPhase := phase.Provisioning
			if op.Kind == storage.OpKindDelete {
				fromPhase = phase.Deleting
			}
			if err := tx.Resources().CASPhase(ctx, *op.ResourceID, fromPhase, phase.Failed, nowMs); err != nil {
				// The resource may legitimately be elsewhere; the phase
				// mapper reconciles later. Never block the journal on it.
				e.log.Warn("failed op: phase not updated", "operation_id", id, "error", err)
			}
		}
		return tx.Events().Append(ctx, &storage.Event{
			TS: nowMs, Type: "operation.failed", OperationID: &id,
			ResourceID: op.ResourceID, Provider: op.Provider,
			Outcome: "failed", Details: json.RawMessage(fmt.Sprintf("%q", cause.Error())),
		})
	})
	if err != nil {
		return err
	}
	e.notifyTerminal(ctx, id)
	return nil
}

// reschedule keeps the operation in its current state with a later
// next_attempt_at (backoff from persisted attempt — ADR-014).
func (e *Engine) reschedule(ctx context.Context, op *storage.Operation, cause error) error {
	delay := e.backoff(op.Attempt, cause)
	next := e.clock.Now().UnixMilli() + delay.Milliseconds()
	return e.st.Tx(ctx, func(tx storage.TxStore) error {
		return tx.Operations().Transition(ctx, op.ID, op.State, op.State, func(o *storage.Operation) {
			o.NextAttemptAt = &next
			if cause != nil {
				setErr(o, provider.Classify(cause), cause)
			}
		})
	})
}

// backoff derives the delay from the persisted attempt count so pacing
// survives restarts (ADR-014): min(cap, base·2^attempt) with full jitter,
// floored by any explicit Retry-After.
func (e *Engine) backoff(attempt int, cause error) time.Duration {
	d := e.cfg.BackoffBase << min(attempt, 16)
	if d > e.cfg.BackoffCap || d <= 0 {
		d = e.cfg.BackoffCap
	}
	d = time.Duration(float64(d) * (0.5 + rand.Float64()))
	if ra, ok := provider.RetryAfterOf(cause); ok && ra > d {
		d = ra
	}
	if d < 10*time.Millisecond {
		d = 10 * time.Millisecond
	}
	return d
}

func (e *Engine) notifyTerminal(ctx context.Context, id storage.OperationID) {
	if e.OnTerminal == nil {
		return
	}
	if op, err := e.st.Operations().Get(ctx, id); err == nil {
		e.OnTerminal(op)
	}
}

func setErr(o *storage.Operation, class provider.ErrorClass, cause error) {
	c := string(class)
	o.ErrorClass = &c
	detail, _ := json.Marshal(cause.Error())
	o.ErrorDetail = detail
}
