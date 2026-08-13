// Package apiclient is the Go client for the Fleetplane HTTP API and the
// home of the wire types (the server serializes exactly these shapes).
// External automation uses this package or plain HTTP — never kernel
// packages (ADR-006, Phase-6 exit).
package apiclient

import (
	"encoding/json"
	"time"
)

const APIVersion = "fleetplane.io/v1alpha1"

// Metadata is the common object metadata (04 §1).
type Metadata struct {
	ID        string            `json:"id"`
	Name      string            `json:"name,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Ownership string            `json:"ownership,omitempty"`
	Protected bool              `json:"protected,omitempty"`

	Generation         int64 `json:"generation,omitempty"`
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	CreatedAt time.Time  `json:"createdAt,omitzero"`
	UpdatedAt time.Time  `json:"updatedAt,omitzero"`
	DeletedAt *time.Time `json:"deletedAt,omitempty"`
}

// ResourceSpec is the resource spec envelope: orchestration fields plus the
// kind-specific machine spec.
type ResourceSpec struct {
	Kind     string          `json:"kind"`
	Provider string          `json:"provider"`
	Class    string          `json:"class,omitempty"`
	Machine  json.RawMessage `json:"machine,omitempty"` // compute.machine spec
}

// ResourceStatus carries orchestration status; Extensions passes
// provider-native data through untouched (invariant 6).
type ResourceStatus struct {
	Phase       string          `json:"phase"`
	ExternalID  string          `json:"externalId,omitempty"`
	ExternalRef json.RawMessage `json:"externalRef,omitempty"`
	Capacity    json.RawMessage `json:"capacity,omitempty"`
	Extensions  json.RawMessage `json:"extensions,omitempty"`
}

// Resource is the wire resource envelope (04 §1).
type Resource struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"` // "Resource"
	Metadata   Metadata       `json:"metadata"`
	Spec       ResourceSpec   `json:"spec"`
	Status     ResourceStatus `json:"status,omitzero"`
}

// ResourceList is the list envelope.
type ResourceList struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"` // "ResourceList"
	Items      []Resource `json:"items"`
}

// CreateResourceRequest is the POST /v1/resources body.
type CreateResourceRequest struct {
	APIVersion string       `json:"apiVersion,omitempty"`
	Kind       string       `json:"kind,omitempty"` // "Resource"
	Metadata   Metadata     `json:"metadata,omitzero"`
	Spec       ResourceSpec `json:"spec"`
}

// ErrorBody is the single error shape every non-2xx response carries.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code              string          `json:"code"`
	Message           string          `json:"message"`
	RequestID         string          `json:"requestId,omitempty"`
	Retryable         bool            `json:"retryable"`
	RetryAfterSeconds int             `json:"retryAfterSeconds,omitempty"`
	Details           json.RawMessage `json:"details,omitempty"`
}
