// Package storage defines the domain-shaped persistence contract (06 §2) —
// the single vocabulary every kernel component compiles against (plan R1,
// I2 contract freeze). Implementations live in storage/sqlite; nothing above
// that package touches SQL.
package storage

import (
	"encoding/json"

	"github.com/samishal1998/fleetplane/internal/phase"
)

// Typed IDs. All are "<prefix>_<ULID>" (ADR-004).
type (
	ResourceID       string
	PoolID           string
	LeaseID          string
	AcquisitionID    string
	OperationID      string
	EventID          string
	ProviderInstance string
)

// Ownership is Fleetplane's authority over a resource (07 §5).
type Ownership string

const (
	OwnershipManaged  Ownership = "managed"  // Fleetplane may mutate/delete
	OwnershipAdopted  Ownership = "adopted"  // per explicit adoption policy
	OwnershipObserved Ownership = "observed" // read-only
)

// OpState is the operation-journal state machine (ADR-017, plan R1) — the
// ONLY operation vocabulary in the system.
//
//	journaled → in_flight → external_accepted → succeeded | failed
//	in_flight → verifying → {external_accepted, journaled, succeeded, uncertain}
//	journaled → aborted
//	uncertain → verifying (manual :resolve only)
type OpState string

const (
	OpJournaled        OpState = "journaled"         // intent committed; provider provably not called
	OpInFlight         OpState = "in_flight"         // dispatch marker committed; call outcome unknown
	OpExternalAccepted OpState = "external_accepted" // provider ref persisted (05 §10)
	OpVerifying        OpState = "verifying"         // post-crash/ambiguous; probing provider
	OpUncertain        OpState = "uncertain"         // verification exhausted; frozen for manual resolve
	OpSucceeded        OpState = "succeeded"
	OpFailed           OpState = "failed"
	OpAborted          OpState = "aborted" // withdrawn before dispatch
)

// Terminal reports whether s is a terminal operation state.
func (s OpState) Terminal() bool {
	return s == OpSucceeded || s == OpFailed || s == OpAborted
}

// NonTerminalOpStates is the set used by partial indexes and recovery scans.
var NonTerminalOpStates = []OpState{OpJournaled, OpInFlight, OpExternalAccepted, OpVerifying, OpUncertain}

// OpKind names what an operation does.
type OpKind string

const (
	OpKindCreate OpKind = "resource.create"
	OpKindDelete OpKind = "resource.delete"
)

// LeaseState: reservations insert active directly (plan R5 — no "reserved").
type LeaseState string

const (
	LeaseActive   LeaseState = "active"
	LeaseReleased LeaseState = "released"
	LeaseExpired  LeaseState = "expired"
)

// AcqState is the acquisition lifecycle (04 §4, kernel design §3.4).
type AcqState string

const (
	AcqPending      AcqState = "pending"
	AcqProvisioning AcqState = "provisioning"
	AcqBound        AcqState = "bound"
	AcqFailed       AcqState = "failed"
	AcqReleased     AcqState = "released"
	AcqExpired      AcqState = "expired"
)

// Resource is the persisted resource record (02 §4). Extension and Capacity
// are stored verbatim (invariant 6). Times are unix-millis UTC.
type Resource struct {
	ID       ResourceID
	Name     string
	Kind     string
	Provider ProviderInstance
	Class    string
	PoolID   *PoolID

	Ownership Ownership
	Phase     phase.Phase

	ExternalID  *string
	ExternalRef json.RawMessage // full ref incl. provider-native addressing

	Generation         int64
	ObservedGeneration int64

	Spec      json.RawMessage
	Extension json.RawMessage // provider-opaque (invariant 6)
	Capacity  json.RawMessage // e.g. {"cpu":4,"memoryMiB":16384}

	ExclusiveAlloc  bool
	DeleteProtected bool
	Labels          map[string]string

	// Reclaim-predicate inputs (plan R4).
	ReadyAt          *int64
	LastLeaseEndedAt *int64
	DrainStartedAt   *int64

	CreatedAt int64
	UpdatedAt int64
	DeletedAt *int64 // tombstone (ADR-017); never a hard DELETE
}

// ResourceFilter narrows List.
type ResourceFilter struct {
	Kind     string
	Provider ProviderInstance
	PoolID   *PoolID
	// Poolless selects resources with no pool (the class idle sweeper's
	// universe, plan R21).
	Poolless  bool
	Class     string
	Phases    []phase.Phase
	Ownership []Ownership
	Labels    map[string]string
	// IncludeDeleted includes tombstoned rows (default false).
	IncludeDeleted bool
}

// ObservedSnapshot is the latest provider observation for one resource
// (observed state is cache — 02 §5).
type ObservedSnapshot struct {
	ResourceID    ResourceID
	ObservedAt    int64
	ProviderPhase string          // normalized ObservedPhase value
	Status        json.RawMessage // structured observation
	Raw           json.RawMessage // full native object (invariant 6)
	ObserveError  string
}

// Operation is one journal entry. Action is the full serialized
// provider.Action, replayable byte-for-byte (invariant 7). The operation ID
// doubles as the provider-side dedup label value (ActionID := OperationID).
type Operation struct {
	ID       OperationID
	ParentID *OperationID

	IdemScope *string
	IdemKey   *string

	Kind       OpKind
	ResourceID *ResourceID
	PoolID     *PoolID
	Provider   ProviderInstance

	Action json.RawMessage
	State  OpState

	Attempt     int
	ExternalOp  json.RawMessage // provider.OperationRef
	ExternalRef json.RawMessage // accept-time external ref (05 §10)

	ErrorClass  *string
	ErrorDetail json.RawMessage

	NextAttemptAt    *int64
	VerifyDeadlineAt *int64

	CreatedAt  int64
	UpdatedAt  int64
	TerminalAt *int64
}

// Lease allocates capacity of one resource to one holder (04 §6).
type Lease struct {
	ID            LeaseID
	AcquisitionID AcquisitionID
	ResourceID    ResourceID
	Holder        string
	Capacity      json.RawMessage
	Exclusive     bool
	State         LeaseState
	ExpiresAt     *int64
	CreatedAt     int64
	EndedAt       *int64
}

// Acquisition is one acquire request (04 §4) with the scale-on-demand
// binding columns of plan R5/R6.
type Acquisition struct {
	ID          AcquisitionID
	Actor       string
	Kind        string
	Class       string
	Constraints json.RawMessage
	Quantity    int
	TTLSeconds  int64
	State       AcqState

	LeaseID           *LeaseID
	PendingResourceID *ResourceID

	CreatedAt int64
	UpdatedAt int64
}

// Pool is a desired-capacity spec (04 §5).
type Pool struct {
	ID                 PoolID
	Name               string
	Kind               string
	Spec               json.RawMessage // class, replicas, minReady, maxResources, reclaim
	Generation         int64
	ObservedGeneration int64
	Paused             bool
	CreatedAt          int64
	UpdatedAt          int64
}

// Event is one append-only audit record (07 §6).
type Event struct {
	ID                EventID
	TS                int64
	Actor             string
	RequestID         string
	IdemKey           string
	Type              string
	ResourceID        *ResourceID
	PoolID            *PoolID
	OperationID       *OperationID
	Provider          ProviderInstance
	Intent            json.RawMessage
	Plan              json.RawMessage
	ProviderRequestID string
	Outcome           string
	Details           json.RawMessage
}

// EventFilter narrows event listing; After is the evt_ ULID cursor (R22).
type EventFilter struct {
	After      EventID
	SinceTS    int64
	Type       string
	ResourceID *ResourceID
	PoolID     *PoolID
}

// IdemState is the idempotency record lifecycle (plan R17).
type IdemState string

const (
	IdemInProgress IdemState = "in_progress"
	IdemCompleted  IdemState = "completed"
	IdemFailed     IdemState = "failed"
)

// IdemRecord stores the replayable outcome of one keyed request: stored
// bytes, replayed byte-identically (invariant 2).
type IdemRecord struct {
	Scope       string
	Key         string
	RequestHash string
	State       IdemState
	OperationID *OperationID
	HTTPStatus  int
	Result      json.RawMessage
	CreatedAt   int64
	CompletedAt *int64
	ExpiresAt   *int64
}

// BeginOutcome is the result of IdempotencyStore.Begin.
type BeginOutcome int

const (
	BeginNew        BeginOutcome = iota // fresh key: proceed, journal in same tx
	BeginInProgress                     // another request holds it: 202 + operation pointer
	BeginCompleted                      // replay stored bytes
	BeginMismatch                       // same key, different request hash: 409
)
