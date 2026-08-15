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
