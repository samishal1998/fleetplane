// Package capacity implements the allocation arithmetic (04 §6):
// dimensioned integer capacity vectors and the constraint shape of 04 §4.
package capacity

import (
	"encoding/json"
	"fmt"
)

// Vector is capacity per dimension, e.g. {"cpu":4,"memoryMiB":16384}.
type Vector map[string]int64

func Parse(raw json.RawMessage) (Vector, error) {
	if len(raw) == 0 {
		return Vector{}, nil
	}
	var v Vector
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("capacity: %w", err)
	}
	return v, nil
}

// Constraints is the request shape of 04 §4: {"cpu":{"min":2}}. v1 requests
// exactly the min of each dimension.
type Constraints map[string]struct {
	Min int64 `json:"min"`
}

// WantFromConstraints converts constraints into the requested capacity.
func WantFromConstraints(raw json.RawMessage) (Vector, error) {
	if len(raw) == 0 {
		return Vector{}, nil
	}
	var c Constraints
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("constraints: %w", err)
	}
	want := Vector{}
	for dim, v := range c {
		if v.Min < 0 {
			return nil, fmt.Errorf("constraints: %s.min must be >= 0", dim)
		}
		if v.Min > 0 {
			want[dim] = v.Min
		}
	}
	return want, nil
}

// Fits reports whether want fits into total minus used. Dimensions the
// resource does not track are conservatively refused when requested.
func Fits(total, used, want Vector) bool {
	for dim, w := range want {
		t, tracked := total[dim]
		if !tracked {
			return false
		}
		if t-used[dim] < w {
			return false
		}
	}
	return true
}

// FreeAfter returns Σ(total − used − want) across the RESOURCE's dimensions
// — the best-fit score (smaller = tighter fit).
func FreeAfter(total, used, want Vector) int64 {
	var free int64
	for dim, t := range total {
		free += t - used[dim] - want[dim]
	}
	return free
}

// Request is the persisted acquisition request payload
// (acquisitions.constraints): capacity constraints, exclusivity, and the
// queueing contract resolved at accept time (docs/11 §8). MaxWaitMs is the
// EFFECTIVE value (request else class default, clamped) — evaluation never
// re-consults config, so a restart resumes exactly the same deadline.
type Request struct {
	Exclusive   bool            `json:"exclusive,omitempty"`
	Constraints json.RawMessage `json:"constraints,omitempty"`
	MaxWaitMs   int64           `json:"maxWaitMs,omitempty"`
}

// ParseRequest decodes the stored payload and derives the capacity want and
// exclusivity (no dimensioned request = whole-machine semantics).
func ParseRequest(raw json.RawMessage) (Request, Vector, bool, error) {
	var req Request
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return req, nil, false, fmt.Errorf("acquisition constraints: %w", err)
		}
	}
	want, err := WantFromConstraints(req.Constraints)
	if err != nil {
		return req, nil, false, err
	}
	exclusive := req.Exclusive || len(want) == 0
	return req, want, exclusive, nil
}
