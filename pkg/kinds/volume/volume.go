// Package volume defines the storage.volume resource kind — the Phase-9
// proof that Resource, capabilities, planning and reconciliation are
// generic rather than VM-shaped (doc 09).
package volume

import (
	"encoding/json"
	"fmt"

	"github.com/samimishal/fleetplane/pkg/kinds"
	"github.com/samimishal/fleetplane/pkg/sdk/provider"
)

// Kind is the resource kind this module defines.
const Kind provider.ResourceKind = "storage.volume"

// DimStorageGiB is the volume capacity dimension.
const DimStorageGiB provider.Dimension = "storageGiB"

func init() {
	kinds.Register(kinds.Descriptor{
		Kind: Kind,
		ValidateSpec: func(raw json.RawMessage) error {
			_, err := ParseSpec(raw)
			return err
		},
		CapacityDims: []provider.Dimension{DimStorageGiB},
	})
}

// Spec is the kind-specific volume spec.
type Spec struct {
	SizeGiB int64  `json:"sizeGiB"`
	Zone    string `json:"zone,omitempty"`
	// Filesystem is provider-interpreted (opaque to the kernel).
	Filesystem string `json:"filesystem,omitempty"`
}

func ParseSpec(raw json.RawMessage) (*Spec, error) {
	var s Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("storage.volume spec: %w", err)
	}
	if s.SizeGiB <= 0 {
		return nil, fmt.Errorf("storage.volume spec: sizeGiB must be > 0")
	}
	return &s, nil
}
