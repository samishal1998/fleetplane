package apiclient

import (
	"encoding/json"
	"time"
)

// AcquireRequest is the POST /v1/acquisitions body (04 §4).
type AcquireRequest struct {
	Kind        string          `json:"kind,omitempty"` // default compute.machine
	Class       string          `json:"class,omitempty"`
	Quantity    int             `json:"quantity,omitempty"`
	Constraints json.RawMessage `json:"constraints,omitempty"` // {"cpu":{"min":2},...}
	Exclusive   bool            `json:"exclusive,omitempty"`
	Lease       *LeaseRequest   `json:"lease,omitempty"`
	// MaxWait (Go duration, e.g. "10m") lets the acquisition queue for
	// existing capacity before scaling up (docs/11 §8); empty falls back
	// to the class default. Requires a server >= v0.4.
	MaxWait string `json:"maxWait,omitempty"`
	// IdempotencyKey may come in the body (04 §4) or the header; when both
	// are set they must agree.
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
}

type LeaseRequest struct {
	TTL string `json:"ttl,omitempty"` // Go duration, e.g. "90m"
}

// Acquisition is the acquisition envelope.
type Acquisition struct {
	APIVersion   string `json:"apiVersion"`
	Kind         string `json:"kind"` // "Acquisition"
	ID           string `json:"id"`
	State        string `json:"state"` // pending|provisioning|bound|failed|released|expired
	Class        string `json:"class,omitempty"`
	ResourceKind string `json:"resourceKind,omitempty"`
	ResourceID   string `json:"resourceId,omitempty"`
	LeaseID      string `json:"leaseId,omitempty"`
	Actor        string `json:"actor,omitempty"`
	// MaxWait/QueueDeadline surface the resolved queue contract (accept-
	// time deterministic, so they are safe in idempotent replays).
	MaxWait       string     `json:"maxWait,omitempty"`
	QueueDeadline *time.Time `json:"queueDeadline,omitempty"`
	CreatedAt     time.Time  `json:"createdAt,omitzero"`
	UpdatedAt     time.Time  `json:"updatedAt,omitzero"`
}

// PoolManifest is the POST/PUT pool body (04 §5): spec is the reconcile
// PoolSpec shape, passed through opaquely.
type PoolManifest struct {
	APIVersion string          `json:"apiVersion,omitempty"`
	Kind       string          `json:"kind,omitempty"` // "Pool"
	Metadata   Metadata        `json:"metadata,omitzero"`
	Spec       json.RawMessage `json:"spec"`
}

type Pool struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"` // "Pool"
	Metadata   Metadata        `json:"metadata"`
	Spec       json.RawMessage `json:"spec"`
	Paused     bool            `json:"paused,omitempty"`
}

type PoolList struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Items      []Pool `json:"items"`
}

type Operation struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	State      string    `json:"state"`
	Provider   string    `json:"provider,omitempty"`
	ResourceID string    `json:"resourceId,omitempty"`
	Attempt    int       `json:"attempt,omitempty"`
	ErrorClass string    `json:"errorClass,omitempty"`
	CreatedAt  time.Time `json:"createdAt,omitzero"`
	UpdatedAt  time.Time `json:"updatedAt,omitzero"`
}

type Event struct {
	ID          string    `json:"id"`
	TS          time.Time `json:"ts"`
	Type        string    `json:"type"`
	Actor       string    `json:"actor,omitempty"`
	Outcome     string    `json:"outcome,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	ResourceID  string    `json:"resourceId,omitempty"`
	OperationID string    `json:"operationId,omitempty"`
}

// ClassSpec is the class template payload (04 §2, dynamic classes).
type ClassSpec struct {
	Kind     string          `json:"kind"`     // resource kind, e.g. compute.machine
	Provider string          `json:"provider"` // provider instance name
	Template json.RawMessage `json:"template"` // kind-specific spec
	Reclaim  *ClassReclaim   `json:"reclaim,omitempty"`
	// Scheduling holds the class's cost/latency tradeoff (docs/11 §8).
	Scheduling *ClassScheduling `json:"scheduling,omitempty"`
}

type ClassReclaim struct {
	IdleAfter string `json:"idleAfter,omitempty"` // Go duration, e.g. "5m"
	// Park: "auto" (default) parks on capable providers; "never" deletes.
	Park string `json:"park,omitempty"`
	// DeleteAfter: delete machines parked this long (poolless classes).
	DeleteAfter string `json:"deleteAfter,omitempty"`
}

type ClassScheduling struct {
	Queue *ClassQueue `json:"queue,omitempty"`
}

type ClassQueue struct {
	MaxWait string `json:"maxWait,omitempty"` // Go duration, e.g. "10m"
}

// ClassManifest is the POST/PUT /v1/classes body.
type ClassManifest struct {
	APIVersion string    `json:"apiVersion,omitempty"`
	Kind       string    `json:"kind,omitempty"` // "Class"
	Metadata   Metadata  `json:"metadata,omitzero"`
	Spec       ClassSpec `json:"spec"`
}

// Class is the class envelope. Source is "config" (file-owned, read-only
// via the API) or "api".
type Class struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"` // "Class"
	Metadata   Metadata  `json:"metadata"`
	Spec       ClassSpec `json:"spec"`
	Source     string    `json:"source"`
}

type ClassList struct {
	APIVersion string  `json:"apiVersion"`
	Kind       string  `json:"kind"`
	Items      []Class `json:"items"`
}
