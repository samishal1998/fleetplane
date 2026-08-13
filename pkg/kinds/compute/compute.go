// Package compute defines the compute.machine resource kind: its spec
// schema (class expansion input), capacity dimensions, and the workload
// readiness probe (08 §4). It is provider- and kernel-agnostic, stdlib-only.
package compute

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/samimishal/fleetplane/pkg/kinds"
	"github.com/samimishal/fleetplane/pkg/sdk/provider"
)

// Kind is the resource kind this module defines.
const Kind provider.ResourceKind = "compute.machine"

// Capacity dimensions for compute.machine.
const (
	DimCPU       provider.Dimension = "cpu"
	DimMemoryMiB provider.Dimension = "memoryMiB"
)

func init() {
	kinds.Register(kinds.Descriptor{
		Kind: Kind,
		ValidateSpec: func(raw json.RawMessage) error {
			_, err := ParseSpec(raw)
			return err
		},
		CapacityDims: []provider.Dimension{DimCPU, DimMemoryMiB},
	})
}

// MachineSpec is the kind-specific spec (the class expansion target, plan
// R15). Provider-specific fields are allowed in classes (04 §2); unknown
// fields round-trip via Extra.
type MachineSpec struct {
	ServerType string            `json:"serverType"`
	Image      string            `json:"image"` // "id:<n>" | "name:<os>" | "snapshot:<label-selector>"
	Location   string            `json:"location,omitempty"`
	UserData   string            `json:"userData,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Readiness  *ReadinessSpec    `json:"readiness,omitempty"`
}

// ParseSpec decodes and validates a machine spec.
func ParseSpec(raw json.RawMessage) (*MachineSpec, error) {
	var s MachineSpec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("compute.machine spec: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *MachineSpec) Validate() error {
	if s.ServerType == "" {
		return fmt.Errorf("compute.machine spec: serverType is required")
	}
	if s.Image == "" {
		return fmt.Errorf("compute.machine spec: image is required")
	}
	if s.Readiness != nil {
		if err := s.Readiness.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ReadinessSpec configures the workload readiness gate executed by the
// operation engine after provider-level success (plan R16). v1 implements
// TCP only; HTTP parses but is rejected at validation (seam kept, ADR).
type ReadinessSpec struct {
	TCP  *TCPProbe  `json:"tcp,omitempty"`
	HTTP *HTTPProbe `json:"http,omitempty"`

	InitialDelay     Duration `json:"initialDelay,omitempty"`
	Period           Duration `json:"period,omitempty"`
	Budget           Duration `json:"budget,omitempty"` // total time before the probe declares failure
	SuccessThreshold int      `json:"successThreshold,omitempty"`
}

type TCPProbe struct {
	Port    int      `json:"port"`
	Timeout Duration `json:"timeout,omitempty"`
}

// HTTPProbe passes when GET http://addr:port/path answers ExpectStatus
// (default: any 2xx).
type HTTPProbe struct {
	Port         int      `json:"port"`
	Path         string   `json:"path"`
	ExpectStatus int      `json:"expectStatus,omitempty"`
	Timeout      Duration `json:"timeout,omitempty"`
}

func (r *ReadinessSpec) Validate() error {
	if r.TCP == nil && r.HTTP == nil {
		return fmt.Errorf("compute.machine readiness: a probe (tcp or http) is required when readiness is set")
	}
	if r.TCP != nil && r.HTTP != nil {
		return fmt.Errorf("compute.machine readiness: declare either tcp or http, not both")
	}
	if r.TCP != nil && (r.TCP.Port <= 0 || r.TCP.Port > 65535) {
		return fmt.Errorf("compute.machine readiness: tcp.port %d out of range", r.TCP.Port)
	}
	if r.HTTP != nil {
		if r.HTTP.Port <= 0 || r.HTTP.Port > 65535 {
			return fmt.Errorf("compute.machine readiness: http.port %d out of range", r.HTTP.Port)
		}
		if r.HTTP.Path == "" || r.HTTP.Path[0] != '/' {
			return fmt.Errorf("compute.machine readiness: http.path must start with /")
		}
	}
	return nil
}

// Defaults applied by the probe executor.
const (
	DefaultProbeTimeout      = 3 * time.Second
	DefaultProbePeriod       = 5 * time.Second
	DefaultProbeBudget       = 5 * time.Minute
	DefaultSuccessThreshold  = 1
	DefaultProbeInitialDelay = 0 * time.Second
)

// Duration is a time.Duration that (un)marshals as a Go duration string.
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}
