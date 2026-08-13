// Package provider is the single normative home of every type shared between
// the Fleetplane kernel and provider implementations (docs/03, ADR-013,
// plan reconciliation R2/R7). Providers import this package (and nothing
// else of Fleetplane); the kernel consumes exactly these identifiers.
package provider

import (
	"context"
	"encoding/json"
	"time"
)

// ResourceKind names resource semantics, e.g. "compute.machine". Kinds are
// defined by kind modules (pkg/kinds); providers declare which they drive.
type ResourceKind string

// CapabilityID names optional provider functionality, e.g.
// "compute.machine.resize" (docs/03 §3).
type CapabilityID string

// Dimension is a capacity axis, e.g. "cpu", "memoryMiB".
type Dimension string

// Capacity is an amount per dimension. Absent dimensions mean "not tracked".
type Capacity map[Dimension]int64

// Provider represents one configured external infrastructure authority
// (docs/03 §2). Implementations must be safe for concurrent use.
type Provider interface {
	Descriptor() Descriptor
	Capabilities(ctx context.Context) ([]CapabilityID, error)
	ResourceDriver(kind ResourceKind) (ResourceDriver, bool)
	Health(ctx context.Context) error
	Close() error
}

// Descriptor identifies a provider instance and its static properties.
type Descriptor struct {
	Driver   string // e.g. "hetzner"
	Instance string // e.g. "hetzner-prod"
	Version  string
	Kinds    []ResourceKind
	// SupportsLabelDiscovery is true when Discover honors label selectors.
	// v1 in-tree drivers always support it; a driver that cannot gets
	// uncertain-freeze semantics after crashes instead of auto-resolution
	// (ADR-013, ADR-017).
	SupportsLabelDiscovery bool
}

// ResourceDriver drives one resource kind at one provider (docs/03 §2).
type ResourceDriver interface {
	Kind() ResourceKind

	// Discover lists resources, fully paginated internally. With
	// ScopeOwned it returns only resources carrying this control plane's
	// ownership labels; Selector adds label-equality filters.
	Discover(ctx context.Context, req DiscoverRequest) ([]ObservedResource, error)

	// Get fetches one resource by external reference. A missing resource
	// is *Error{Class: ErrNotFound} — never (zero, nil).
	Get(ctx context.Context, ref ExternalRef) (ObservedResource, error)

	// Plan computes the provider actions that converge Observed toward
	// Desired. Pure: no provider mutation, no side effects.
	Plan(ctx context.Context, req PlanRequest) (Plan, error)

	// Apply executes one journaled action. The returned OperationRef's
	// accept-time external ref must be set as soon as the provider
	// assigns identity (05 §10) — the kernel persists it immediately.
	Apply(ctx context.Context, action Action) (OperationRef, error)

	// ObserveOperation polls a provider-native operation to a terminal
	// state. It must poll by reference, never by listing (05 §10).
	ObserveOperation(ctx context.Context, op OperationRef) (OperationStatus, error)
}

// DiscoverScope selects the discovery universe.
type DiscoverScope string

const (
	// ScopeOwned lists only resources owned by this control plane
	// (managed + owner labels). The default.
	ScopeOwned DiscoverScope = "owned"
	// ScopeAll lists everything visible, for adoption/observation flows.
	ScopeAll DiscoverScope = "all"
)

type DiscoverRequest struct {
	Scope    DiscoverScope
	Selector map[string]string // extra label equality filters
}

// ExternalRef addresses one provider resource. Extra carries provider
// addressing context (zone, project, ...) opaque to the kernel.
type ExternalRef struct {
	ID    string            `json:"id"`
	Extra map[string]string `json:"extra,omitempty"`
}

// ObservedPhase is the normalized provider-side lifecycle phase (plan R7).
// The kernel's phase mapper consumes exactly this enum.
type ObservedPhase string

const (
	PhasePending  ObservedPhase = "pending"
	PhaseRunning  ObservedPhase = "running"
	PhaseStopped  ObservedPhase = "stopped"
	PhaseDeleting ObservedPhase = "deleting"
	PhaseGone     ObservedPhase = "gone"
	PhaseUnknown  ObservedPhase = "unknown"
)

// Address is a network address of a resource, used by readiness probes.
type Address struct {
	Network string // "public-v4" | "public-v6" | "private"
	Addr    string
}

// ObservedResource is one provider-side observation. Extensions carries the
// full native object verbatim and is never dropped (invariant 6).
type ObservedResource struct {
	Ref           ExternalRef
	Kind          ResourceKind
	FleetplaneID  string // from LabelID; "" = foreign/unlabeled
	CreateOpID    string // from LabelOp; the create-dedup anchor (R2)
	Owned         bool   // carries this control plane's owner labels
	Phase         ObservedPhase
	ProviderState string // native status verbatim
	Capacity      Capacity
	Addresses     []Address
	Labels        map[string]string
	Extensions    json.RawMessage
	ObservedAt    time.Time
}

// DesiredState is what Plan converges toward. Spec is kind-specific and
// class-expanded by the kind module; Labels are the kernel-composed
// ownership/identity labels drivers must apply verbatim (R2).
type DesiredState struct {
	Name   string
	Spec   json.RawMessage
	Labels map[string]string
}

type PlanRequest struct {
	ResourceID string
	Desired    *DesiredState     // nil => converge to absence
	Observed   *ObservedResource // nil => not observed
}

type Plan struct {
	Actions []Action // ordered; empty => converged
	Summary []string // human-readable diff lines for audit/events
}

// Action is one journal-safe provider mutation: it JSON round-trips exactly,
// and Apply receives precisely what was persisted so crash-resume replays
// from the journal with no in-memory context (06 §4, invariant 7).
//
// ActionID is the operation ID (ActionID := OperationID, plan R2) — one ULID
// that is simultaneously the journal key and the create-dedup label value.
type Action struct {
	ActionID    string          `json:"actionId"`
	Kind        string          `json:"kind"` // driver-defined: "create", "delete", ...
	ResourceID  string          `json:"resourceId"`
	Ref         *ExternalRef    `json:"ref,omitempty"`    // nil for create
	Params      json.RawMessage `json:"params,omitempty"` // driver-private payload
	Destructive bool            `json:"destructive"`      // gates ownership/policy checks (invariant 5)
}

// OperationRef identifies an in-flight provider operation. Data is driver
// resume state (self-versioned); it may be lost across crashes — drivers
// MUST tolerate Data == nil by degrading to resource-status observation.
type OperationRef struct {
	ActionID string          `json:"actionId"`
	Ref      *ExternalRef    `json:"ref,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// OperationState is the provider-operation polling state (plan R7).
type OperationState string

const (
	OpPending   OperationState = "pending"
	OpRunning   OperationState = "running"
	OpSucceeded OperationState = "succeeded"
	OpFailed    OperationState = "failed"
	OpUnknown   OperationState = "unknown"
)

type OperationStatus struct {
	State      OperationState
	Ref        *ExternalRef      // set as soon as known
	Resource   *ObservedResource // optional fresh snapshot
	Failure    *Error            // set when State == OpFailed
	RetryAfter time.Duration     // next-poll hint; 0 = engine default
}
