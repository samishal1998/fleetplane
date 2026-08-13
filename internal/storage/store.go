package storage

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/samimishal/fleetplane/internal/phase"
)

// Sentinel errors. Implementations wrap these; callers use errors.Is.
var (
	ErrNotFound = errors.New("storage: not found")
	// ErrConflict: uniqueness violation or CAS from-state mismatch.
	ErrConflict = errors.New("storage: conflict")
	// ErrStale: generation CAS failure on spec updates.
	ErrStale = errors.New("storage: stale generation")
)

// Store is the root persistence handle (06 §2 + plan R23). Reads outside a
// transaction use the reader pool; Tx serializes writes on the single writer
// connection with BEGIN IMMEDIATE.
type Store interface {
	TxStore // autocommit reads

	// Tx runs fn in one write transaction (BEGIN IMMEDIATE). Nesting is a
	// programming error and panics.
	Tx(ctx context.Context, fn func(TxStore) error) error
	// View runs fn on a read-only snapshot.
	View(ctx context.Context, fn func(TxStore) error) error

	Ping(ctx context.Context) error
	// Backup writes a consistent compacted copy via VACUUM INTO on a
	// dedicated connection (never the writer) — plan R23.
	Backup(ctx context.Context, destPath string) error
	Close() error
}

// TxStore exposes the domain sub-stores, all operating in one context
// (a transaction inside Tx/View, autocommit otherwise).
type TxStore interface {
	Providers() ProviderInstanceStore
	Resources() ResourceStore
	Pools() PoolStore
	Acquisitions() AcquisitionStore
	Leases() LeaseStore
	Operations() OperationStore
	Idempotency() IdempotencyStore
	Events() EventStore
	Checkpoints() CheckpointStore
}

// ProviderInstanceRecord persists configured provider instances (config
// references only — never resolved credentials).
type ProviderInstanceRecord struct {
	Name      ProviderInstance
	Driver    string
	Config    json.RawMessage
	Enabled   bool
	CreatedAt int64
	UpdatedAt int64
}

type ProviderInstanceStore interface {
	Upsert(ctx context.Context, r *ProviderInstanceRecord) error
	Get(ctx context.Context, name ProviderInstance) (*ProviderInstanceRecord, error)
	List(ctx context.Context) ([]*ProviderInstanceRecord, error)
}

type ResourceStore interface {
	Create(ctx context.Context, r *Resource) error
	Get(ctx context.Context, id ResourceID) (*Resource, error)
	GetByExternalID(ctx context.Context, p ProviderInstance, extID string) (*Resource, error)
	List(ctx context.Context, f ResourceFilter) ([]*Resource, error)

	// UpdateSpec bumps Generation; ErrStale if expectGen doesn't match.
	UpdateSpec(ctx context.Context, r *Resource, expectGen int64) error

	// CASPhase transitions phase with a from-phase predicate (plan R4):
	// ErrConflict when the row is not currently in `from`. The write also
	// maintains ready_at / drain_started_at side columns.
	CASPhase(ctx context.Context, id ResourceID, from, to phase.Phase, atMillis int64) error

	SetObservedGeneration(ctx context.Context, id ResourceID, gen int64) error
	SetExternalRef(ctx context.Context, id ResourceID, extID string, ref json.RawMessage) error
	SetLastLeaseEnded(ctx context.Context, id ResourceID, atMillis int64) error

	// MarkDeleted tombstones (ADR-017). Rows are never hard-deleted.
	MarkDeleted(ctx context.Context, id ResourceID, atMillis int64) error

	PutObserved(ctx context.Context, snap *ObservedSnapshot) error
	GetObserved(ctx context.Context, id ResourceID) (*ObservedSnapshot, error)
}

// PoolStore persists pool specs and reconciler checkpoints.
type PoolStore interface {
	Upsert(ctx context.Context, p *Pool) error
	Get(ctx context.Context, id PoolID) (*Pool, error)
	GetByName(ctx context.Context, name string) (*Pool, error)
	List(ctx context.Context) ([]*Pool, error)
}

type AcquisitionStore interface {
	Insert(ctx context.Context, a *Acquisition) error
	Get(ctx context.Context, id AcquisitionID) (*Acquisition, error)
	// Transition CASes on from-state; ErrConflict on mismatch.
	Transition(ctx context.Context, id AcquisitionID, from, to AcqState, atMillis int64) error
	// Bind links the satisfied acquisition to its lease in the reservation
	// transaction (plan R5); the resource is reachable via the lease.
	Bind(ctx context.Context, id AcquisitionID, lease LeaseID, atMillis int64) error
	SetPendingResource(ctx context.Context, id AcquisitionID, res ResourceID) error
	// ListByState powers acquisition crash-resume (plan R6).
	ListByState(ctx context.Context, states ...AcqState) ([]*Acquisition, error)
}

type LeaseStore interface {
	Insert(ctx context.Context, l *Lease) error
	Get(ctx context.Context, id LeaseID) (*Lease, error)
	ActiveByResource(ctx context.Context, id ResourceID) ([]*Lease, error)
	CountActive(ctx context.Context, id ResourceID) (int, error)
	// SumActive aggregates active lease capacity per dimension — the only
	// allocation truth (plan R4: no allocated cache).
	SumActive(ctx context.Context, id ResourceID) (map[string]int64, error)
	// Transition CASes on from-state and stamps EndedAt for terminal states.
	Transition(ctx context.Context, id LeaseID, from, to LeaseState, atMillis int64) error
	Expiring(ctx context.Context, nowMillis int64, limit int) ([]*Lease, error)
}

type OperationStore interface {
	// Append journals a new operation; State must be OpJournaled. Enforces
	// at most one non-terminal operation per resource (ErrConflict).
	Append(ctx context.Context, op *Operation) error
	Get(ctx context.Context, id OperationID) (*Operation, error)
	// Transition CASes on from-state; mut edits the operation inside the
	// same statement scope (attempt, refs, error, deadlines).
	Transition(ctx context.Context, id OperationID, from, to OpState, mut func(*Operation)) error
	// Due returns dispatchable operations (state runnable, next_attempt_at
	// <= now), oldest first.
	Due(ctx context.Context, nowMillis int64, limit int) ([]*Operation, error)
	// NonTerminal powers the startup recovery scan (invariant 7).
	NonTerminal(ctx context.Context) ([]*Operation, error)
	// CountPending exists as a cross-check assertion only (plan R20):
	// deficit math counts provisioning resource rows.
	CountPending(ctx context.Context, pool PoolID, kind OpKind) (int, error)
}

type IdempotencyStore interface {
	// Begin atomically claims (scope, key) for requestHash. BeginNew
	// registers the key (in the caller's open transaction — key
	// registration and journaled intent commit together, invariant 2).
	Begin(ctx context.Context, scope, key, requestHash string, op OperationID) (BeginOutcome, *IdemRecord, error)
	// Complete/Fail store the replayable outcome; called in the same
	// transaction as the side-effect's final state (plan R17).
	Complete(ctx context.Context, scope, key string, httpStatus int, result json.RawMessage, expiresAt int64) error
	Fail(ctx context.Context, scope, key string, httpStatus int, result json.RawMessage, expiresAt int64) error
	// GC removes expired records whose operation is terminal.
	GC(ctx context.Context, nowMillis int64, limit int) (int, error)
}

type EventStore interface {
	// Append is insert-only; events are never updated or deleted.
	Append(ctx context.Context, ev *Event) error
	// List returns events after f.After (exclusive), oldest first.
	List(ctx context.Context, f EventFilter, limit int) ([]*Event, error)
}

type CheckpointStore interface {
	Get(ctx context.Context, controller string) (json.RawMessage, error)
	Put(ctx context.Context, controller string, cp json.RawMessage, atMillis int64) error
}
